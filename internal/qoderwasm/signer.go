// signer.go 对外的主 API：把一次推理请求编成加密端点的 (URL, body, headers)。
//
// # 它实现的接缝
//
// `qoder.RequestSigner` 的方法形状是：
//
//	BuildInferRequest(id SignIdentity, model string, body []byte)
//	    (path, payload string, headers map[string]string, err error)
//
// 装配层用一个**薄适配器**把 `qoder.SignIdentity` 转成本包的 `Identity`
// （见 cmd/server 的 qoderSigner）。为什么不在本包直接收 `qoder.SignIdentity`：
//
//	本包**不能 import internal/qoder** —— 那个包依赖 internal/gateway，
//	而 arch_test 的 discoverUpstreams 用 `go list -deps`（**传递**依赖）判定
//	"谁消费契约"，一旦本包依赖它就会被判成一个没有 Provider 方法的上游 → 红。
//
// 适配器放在装配层是既有惯例（上游类型 ≠ 核心类型，方法集精确匹配，
// 必须显式转换）。
//
// # 一个实例服务**所有**账号
//
// WASM 模块（298 KB 的编译结果）只建一次，全局共享；每个账号各有一份
// `QoderContext`（它绑定了该账号的 uid 与鉴权字段，**不能跨账号复用** ——
// 拿 A 的 context 构造 B 的请求会得到"签名与身份不符"）。
package qoderwasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Identity 加密签名所需的账号身份。
//
// 纯标量，故意不认识 internal/qoder 的任何类型（见文件头）。
type Identity struct {
	// UID 账号主键（进 Cosy-User 与鉴权字段）。
	UID string
	// AccessToken 访问令牌（WASM 用它算 encrypt_user_info）。
	AccessToken string
	// MachineID 机器标识（进 Cosy-MachineId / Cosy-MachineToken）。
	MachineID string
}

// Options 建签名器的参数（与账号无关的部分）。
type Options struct {
	// Host 加密推理端点 host（如 `api2.qoder.sh`）。
	//
	// ⚠ 必须是 api2.qoder.sh 系 —— 传 api2-v2.qoder.sh 会 404
	//（那是公开 OpenAI 兼容端点的 host，两者不同）。
	Host string
	// ClientType / BusinessProduct / BusinessType / Scene 客户端元数据。
	//
	// 零值用包内默认（`5` / `cli` / `agent` / `assistant`），
	// 与设备码授权用的是同一套 —— 身份要一致，否则服务端会当成
	// 两个不同的客户端。
	ClientType      string
	BusinessProduct string
	BusinessType    string
	Scene           string
	// ClientVersion 客户端版本（影响 `Cosy-Version` 与签名载荷）。
	ClientVersion string
	// OrganizationID / OrganizationTags / DataPolicyAgreed 组织信息。
	OrganizationID   string
	OrganizationTags []string
	DataPolicyAgreed bool
}

// defaultClientVersion `Cosy-Version` 的缺省值。
//
// ⚠ 这个值进**签名载荷**：服务端用同样的串去验签，所以改它必须与
// WASM 的期望一致。
const defaultClientVersion = "1.1.49"

// Signer 加密请求构造器（一个进程一个，服务所有账号）。
type Signer struct {
	mu   sync.Mutex
	mod  *Module
	host string
	opts Options

	// accounts uid → 该账号的 context。
	accounts map[string]*account

	// memLimit 触发重建的 WASM 内存上限（字节）。
	//
	// 0 = 用 memLimitBytes。**测试可注入更小的值**：
	// 生产阈值 64 MB 要一万三千次调用才到，测试里跑不动，
	// 而"到阈值就重建"这条因果链必须被验证。
	memLimit uint32
}

// account 一个账号的 WASM 状态。
type account struct {
	id   Identity
	ctx  uint32
	meta string
}

// memLimitBytes 触发重建的 WASM 内存上限。
//
// # 为什么需要重建（实测的固有泄漏）
//
// 这个 WASM 的设计假设是**有 GC 的宿主**：它把每次调用的入参指针留在
// 内部结构里，从不主动释放，等宿主回收。实测曲线（约 5 KB/次，线性）：
//
//	调用次数    WASM 内存
//	    ~50      1.3 MB
//	   ~200      2.1 MB
//	   ~800      5.3 MB
//	  ~1600      9.6 MB
//
// 而且**不能靠我们归还来消除** —— 三种做法都实测失败：
//
//	归还入参字符串      → 第二次调用 out of bounds（result 指向已回收内存）
//	读完 result 后再还  → 同上（context 跨调用持有指针）
//	复用一块缓冲        → 第一次调用就 unreachable（WASM 按独立分配算边界）
//
// 所以正确做法不是"想办法还"，而是**定期换一个干净实例**：
// 内存到阈值就把整个 WASM 实例（连同所有账号的 context）重建一次。
//
// 取 64 MB：以 5 KB/次算约 1.3 万次调用触发一次，而重建只花几十毫秒。
const memLimitBytes = 64 << 20

// NewSigner 建一个签名器（尚未绑任何账号）。
//
// 这里**不**编译 WASM —— 编译推迟到第一次真正要用时（见 ensureModule）。
// 理由：qoder 启用但一个账号都没有的部署很常见（刚配好、还没登录），
// 那时没必要付 298 KB 编译的启动开销。
func NewSigner(opts Options) (*Signer, error) {
	if strings.TrimSpace(opts.Host) == "" {
		return nil, errors.New("qoderwasm: 缺少加密推理 host")
	}
	return &Signer{
		host:     strings.TrimPrefix(strings.TrimSpace(opts.Host), "https://"),
		opts:     opts,
		accounts: map[string]*account{},
	}, nil
}

// Close 释放 WASM 资源（幂等）。
func (s *Signer) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = map[string]*account{}
	if s.mod == nil {
		return nil
	}
	m := s.mod
	s.mod = nil
	return m.Close(ctx)
}

// effectiveMemLimit 返回生效的内存阈值。
func (s *Signer) effectiveMemLimit() uint32 {
	if s.memLimit > 0 {
		return s.memLimit
	}
	return memLimitBytes
}

// ensureModuleLocked 确保 WASM 实例就绪（调用方须持锁）。
func (s *Signer) ensureModuleLocked() error {
	if s.mod != nil && !s.mod.closed {
		return nil
	}
	m, err := New(context.Background())
	if err != nil {
		return err
	}
	s.mod = m
	s.accounts = map[string]*account{}
	return nil
}

// accountLocked 取（或建）某账号的 context（调用方须持锁）。
func (s *Signer) accountLocked(id Identity) (*account, error) {
	if a, ok := s.accounts[id.UID]; ok {
		return a, nil
	}
	a := &account{id: id}
	if err := s.initAccountLocked(a); err != nil {
		return nil, err
	}
	s.accounts[id.UID] = a
	return a, nil
}

// initAccountLocked 为账号生成鉴权字段并建 QoderContext（调用方须持锁）。
func (s *Signer) initAccountLocked(a *account) error {
	o := s.opts
	id := a.id

	// ---- 1. 鉴权字段（服务端据此识别用户身份）----
	userPayload, err := json.Marshal(map[string]any{
		"uid":                  id.UID,
		"security_oauth_token": id.AccessToken,
		"organization_id":      o.OrganizationID,
		"organization_tags":    orEmptySlice(o.OrganizationTags),
		"data_policy_agreed":   o.DataPolicyAgreed,
	})
	if err != nil {
		return err
	}
	fieldsRaw, err := s.mod.callStringWithStrings(s.mod.fnGenFields, string(userPayload))
	if err != nil {
		return fmt.Errorf("qoderwasm: 生成鉴权字段失败: %w", err)
	}

	var fields struct {
		EncryptUserInfo string `json:"encrypt_user_info"`
		Key             string `json:"key"`
	}
	if err := json.Unmarshal([]byte(fieldsRaw), &fields); err != nil {
		return fmt.Errorf("qoderwasm: 鉴权字段不是合法 JSON: %w（原文 %.120q）", err, fieldsRaw)
	}
	if fields.EncryptUserInfo == "" || fields.Key == "" {
		return fmt.Errorf("qoderwasm: 鉴权字段为空（uid=%s）—— WASM 没算出身份", id.UID)
	}

	// ---- 2. QoderContext ----
	userInfo, err := json.Marshal(map[string]any{
		"uid":                id.UID,
		"encrypt_user_info":  fields.EncryptUserInfo,
		"key":                fields.Key,
		"organization_id":    o.OrganizationID,
		"organization_tags":  orEmptySlice(o.OrganizationTags),
		"data_policy_agreed": o.DataPolicyAgreed,
	})
	if err != nil {
		return err
	}
	version := o.ClientVersion
	if version == "" {
		version = defaultClientVersion
	}
	meta := map[string]string{
		"client_type":      orDefault(o.ClientType, "5"),
		"business_product": orDefault(o.BusinessProduct, "cli"),
		"business_type":    orDefault(o.BusinessType, "agent"),
		"scene":            orDefault(o.Scene, "assistant"),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	a.meta = string(metaJSON)

	// 参数顺序：(sp, machineId, len, version, len, userInfo, len, clientMeta, len)
	ctxHandle, err := s.mod.callPointerWithStrings(s.mod.fnCtxNew,
		id.MachineID, version, string(userInfo), a.meta)
	if err != nil {
		return fmt.Errorf("qoderwasm: 建 QoderContext 失败: %w", err)
	}
	if ctxHandle == 0 {
		return errors.New("qoderwasm: QoderContext 句柄为空")
	}
	a.ctx = ctxHandle
	return nil
}

// recycleLocked 内存超限时重建整个实例（调用方须持锁）。
//
// 重建会丢掉所有账号的 context，所以重建后要把它们**逐个重建回来** ——
// 漏掉任何一个都会让那个账号下一次请求失败（而其它账号正常，
// 表现是"某个账号时好时坏"，很难归因）。
//
// 失败时**不报错**：当前实例仍可用，只是内存偏高 ——
// 为"内存优化"打断一次正常推理是更糟的取舍。下次调用再试。
func (s *Signer) recycleLocked() {
	if s.mod == nil || s.mod.closed {
		return
	}
	if s.mod.mod.Memory().Size() < s.effectiveMemLimit() {
		return
	}
	ctx := context.Background()

	old := s.mod
	oldAccounts := s.accounts

	mod, err := New(ctx)
	if err != nil {
		return // 保留旧实例，下次再试
	}
	// 先在新实例上把所有账号重建回来；任一个失败就整体放弃重建
	//（否则会得到一个"部分账号可用"的中间态）。
	s.mod = mod
	s.accounts = map[string]*account{}
	for uid, a := range oldAccounts {
		na := &account{id: a.id}
		if err := s.initAccountLocked(na); err != nil {
			// 回滚：把旧的接回去，丢掉新建的。
			s.mod = old
			s.accounts = oldAccounts
			_ = mod.Close(ctx)
			return
		}
		s.accounts[uid] = na
	}
	_ = old.Close(ctx)
}

// BuildInferRequest 构造加密推理请求。
//
// 返回的 headers **必须原样透传**：其中的 `Authorization` 是 WASM 生成的
// `Bearer COSY.<载荷>.<签名>`。用普通 `Bearer <token>` 覆盖会导致
// `403 Signature invalid`。
//
// # body 参数怎么用
//
// 入参 body 是**上游（OpenAI 形状）的请求体**，不是加密端点的载荷 ——
// 本方法负责把它拆成 Qoder 的私有形状。所以这里要做一次结构转换，
// 而不是把 body 原样送进 WASM。
func (s *Signer) BuildInferRequest(id Identity, model string, body []byte) (string, []byte, map[string]string, error) {
	if s == nil {
		return "", nil, nil, errors.New("qoderwasm: 签名器为空")
	}
	if strings.TrimSpace(id.UID) == "" {
		return "", nil, nil, errors.New("qoderwasm: 缺少账号 uid（鉴权字段由它算出）")
	}
	ask, err := parseOpenAIBody(model, body)
	if err != nil {
		return "", nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureModuleLocked(); err != nil {
		return "", nil, nil, err
	}
	// 内存超限就换一个干净实例（这个 WASM 靠宿主 GC 回收，我们只能定期重建）。
	s.recycleLocked()

	acc, err := s.accountLocked(id)
	if err != nil {
		return "", nil, nil, err
	}

	payload, err := buildPayload(ask)
	if err != nil {
		return "", nil, nil, err
	}

	// 参数顺序：
	// (sp, context, host, hostLen, body, bodyLen, modelKey, keyLen, source, srcLen)
	//
	// ⚠ **不归还**这四个字符串，也**不能复用缓冲**（两种做法都实测失败）。
	//
	// 归还 → 第二次调用就 `out of bounds`（WASM 把指针留在 result 里）。
	// 复用 → 第一次调用就 `unreachable`（WASM 按"独立分配"算块边界）。
	//
	// 所以每次新分配，靠定期重建实例回收内存（见 memLimitBytes）。
	hostPtr, hostLen, err := s.mod.writeString(s.host)
	if err != nil {
		return "", nil, nil, err
	}
	bodyPtr, bodyLen, err := s.mod.writeString(string(payload))
	if err != nil {
		return "", nil, nil, err
	}
	keyPtr, keyLen, err := s.mod.writeString(ask.ModelKey)
	if err != nil {
		return "", nil, nil, err
	}
	srcPtr, srcLen, err := s.mod.writeString("system")
	if err != nil {
		return "", nil, nil, err
	}

	result, err := s.mod.callPointer(s.mod.fnPrepare,
		uint64(acc.ctx),
		uint64(hostPtr), uint64(hostLen),
		uint64(bodyPtr), uint64(bodyLen),
		uint64(keyPtr), uint64(keyLen),
		uint64(srcPtr), uint64(srcLen))
	if err != nil {
		return "", nil, nil, fmt.Errorf("qoderwasm: 构造加密请求失败: %w", err)
	}

	headers, err := s.headersOf(result)
	if err != nil {
		s.releaseResult(result)
		return "", nil, nil, err
	}

	// ⚠ 用 callResultString 而不是 callString：返回值布局不同（无错误槽）。
	url, err := s.mod.callResultString(s.mod.fnURL, result)
	if err != nil {
		s.releaseResult(result)
		return "", nil, nil, fmt.Errorf("qoderwasm: 取 URL 失败: %w", err)
	}
	encBody, err := s.mod.callResultString(s.mod.fnBody, result)
	if err != nil {
		s.releaseResult(result)
		return "", nil, nil, fmt.Errorf("qoderwasm: 取请求体失败: %w", err)
	}

	// result 读完即释放 —— 它同时也是"入参字符串不再被引用"的信号。
	s.releaseResult(result)

	if !strings.Contains(url, encryptedInferMarker) {
		return "", nil, nil, fmt.Errorf("qoderwasm: WASM 给出的 URL 不是加密推理端点（%.120q）—— "+
			"host 或模型 key 不对", url)
	}
	// URL 的 query 部分由调用方拼到加密端点上。
	path := ""
	if i := strings.IndexByte(url, '?'); i >= 0 {
		path = url[i:]
	}
	return path, []byte(encBody), headers, nil
}

// encryptedInferMarker 加密推理端点的路径特征（用于校验 WASM 的产物）。
const encryptedInferMarker = "/algo/api/v2/service/pro/sse/agent_chat_generation"

// releaseResult 释放一个 requestresult。
//
// # 为什么可以释放（实测）
//
// 与入参字符串不同，result 只在本次调用内被读（headers / url / body），
// 读完就没有别的引用了。实测：释放后连续 5 次调用全部成功。
//
// ⚠ 参数顺序：参照实现从不调这个函数（它靠 JS GC），所以没有可对照的
// 调用点。实测两种顺序（`(result, 0)` 与 `(0, result)`）都能跑通 ——
// 说明其中一个参数是"对齐/尺寸"类的次要参数。这里用 `(0, result)`，
// 与 `__wbg_qodercontext_free` 的写法保持一致。
func (s *Signer) releaseResult(result uint32) {
	if result == 0 || s.mod == nil || s.mod.mod == nil {
		return
	}
	f := s.mod.mod.ExportedFunction("__wbg_requestresult_free")
	if f == nil {
		return
	}
	_, _ = f.Call(context.Background(), 0, uint64(result))
}

// headersOf 从 requestresult 取请求头。
//
// ⚠ `requestresult_headers(result)` 收的是 result **句柄**，返回的是
// **对象堆索引**。把 result 当对象索引去查会越界。
func (s *Signer) headersOf(result uint32) (map[string]string, error) {
	res, err := s.mod.fnHdr.Call(context.Background(), uint64(result))
	if err != nil {
		return nil, fmt.Errorf("qoderwasm: 取请求头失败: %w", err)
	}
	idx := uint32(res[0])
	v := s.mod.take(idx)
	if v.kind != kindMap || len(v.m) == 0 {
		// 空 map 是**功能级故障**：没有 Authorization 就一定 403。
		// 报错而不是返回空 map —— 后者会让调用方发一个必然失败的请求。
		return nil, fmt.Errorf("qoderwasm: WASM 未产出请求头（kind=%d, n=%d）—— "+
			"签名头缺失必然 403", v.kind, len(v.m))
	}
	return v.m, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orEmptySlice(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
