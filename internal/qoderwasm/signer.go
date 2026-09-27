// signer.go 对外的主 API：把一次推理请求编成加密端点的 (URL, body, headers)。
//
// # 它实现的接缝
//
// `qoder.RequestSigner` 只有一个方法：
//
//	BuildInferRequest(model string, body []byte) (path, payload, headers, error)
//
// 装配层把本包的对象注入 `qoder.Config.Signer`，qoder 的网络层只管把它
// 产出的东西发出去。这样"加密怎么做"与"怎么发请求"各自可独立测试 ——
// 前者用固定输入验证（本包），后者用假上游验证（internal/qoder）。
package qoderwasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Signer 一个账号的加密请求构造器。
//
// # ⚠ 一个账号一个实例
//
// WASM 的 `QoderContext` 在构造时**绑定了该账号的 uid 与鉴权字段**
// （`generate_runtime_auth_fields` 的产物）。拿 A 账号的 context 去构造
// B 账号的请求会得到"签名与身份不符"的失败 —— 所以不能全局复用一个。
//
// 代价是每个账号要实例化一次 WASM 上下文（不重新编译模块，只调
// `qodercontext_new`，微秒级）。模块本身（298 KB 编译结果）是全局共享的。
type Signer struct {
	mod *Module

	// host 加密推理端点所在 host（**必须** api2.qoder.sh 系）。
	host string
	// ctx 该账号的 QoderContext 句柄。
	ctx uint32
	// meta 客户端元数据 JSON。
	meta string

	// mu 串行化 prepareInfer。
	//
	// 为什么必须：WASM 的对象堆是**共享可变状态**，而 prepareInfer
	// 会 push/take 对象索引。两个 goroutine 同时调会让索引互相踩
	//（表象是随机的签名错误，只在并发时复现 —— 极难定位）。
	// 而 QoderContext 本身也不是线程安全的（参照实现明说
	// "实例不是线程安全的；一个账号一个实例即可"）。
	mu sync.Mutex

	// opts 建 context 用的原始参数 —— 回收重建时要原样复用。
	opts SignerOptions

	// calls 自上次重建以来的调用次数。
	calls int

	// memLimit 触发重建的 WASM 内存上限（字节）。
	//
	// 0 = 用 memLimitBytes。**测试可注入更小的值**：
	// 生产阈值 64 MB 要一万三千次调用才到，测试里跑不动，
	// 而"到阈值就重建"这条因果链必须被验证。
	memLimit uint32
}

// effectiveMemLimit 返回生效的内存阈值。
func (s *Signer) effectiveMemLimit() uint32 {
	if s.memLimit > 0 {
		return s.memLimit
	}
	return memLimitBytes
}

// memLimitBytes 触发重建的 WASM 内存上限。
//
// # 为什么需要重建（实测的固有泄漏）
//
// 这个 WASM 的设计假设是**有 GC 的宿主**：它把每次调用的入参指针留在
// 内部结构里，从不主动释放，等宿主回收。实测证据：
//
//	调用次数    WASM 内存
//	    ~50      1.3 MB
//	   ~200      2.1 MB
//	   ~800      5.3 MB
//	  ~1600      9.6 MB      ← 线性，约 5 KB/次
//
// 而且**不能靠我们归还来消除**（三种尝试都失败，各有实测）：
//
//	归还入参字符串      → 第二次调用就 out of bounds（result 指向已回收内存）
//	读完 result 后再还  → 同上（context 跨调用持有指针）
//	复用一块缓冲        → 第一次调用就 unreachable（WASM 按独立分配算边界）
//
// 所以正确做法不是"想办法还"，而是**定期换一个干净实例**：
// 内存到阈值就把整个 WASM 实例（连同 context）重建一次。
//
// 取 64 MB：以 5 KB/次算约 1.3 万次调用，远小于 32 位地址空间的 4 GB，
// 而重建一次只花几十毫秒（只编译一次模块，见 New 的注释）。
const memLimitBytes = 64 << 20

// recycleLocked 在内存超限时重建实例（调用方须持锁）。
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
	mod, err := New(ctx)
	if err != nil {
		return // 保留旧实例，下次再试
	}
	fresh := &Signer{mod: mod, host: s.host, opts: s.opts}
	if err := fresh.init(ctx, s.opts); err != nil {
		_ = mod.Close(ctx)
		return
	}
	// 换新成功后释放旧的（顺序要紧：先建好新的再关旧的，
	// 否则中途失败就没有可用实例了）。
	s.mod, s.ctx = fresh.mod, fresh.ctx
	s.calls = 0
	_ = old.Close(ctx)
}

// SignerOptions 构造一个账号的签名器。
type SignerOptions struct {
	// UID 账号 uid（进鉴权字段与请求头）。
	UID string
	// SecurityOAuthToken 安全令牌（WASM 用它算 encrypt_user_info）。
	SecurityOAuthToken string
	// MachineID 设备标识。官方用硬件指纹，这里用持久化的随机 UUID。
	MachineID string
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
// WASM 的期望一致。参照实现用的是它自己那份客户端版本（1.1.49）。
const defaultClientVersion = "1.1.49"

// NewSigner 为一个账号构造签名器。
//
// ctx 只用于建 WASM 上下文。
func NewSigner(ctx context.Context, opts SignerOptions) (*Signer, error) {
	if opts.UID == "" {
		return nil, errors.New("qoderwasm: 缺少 uid（鉴权字段由它算出）")
	}
	if opts.Host == "" {
		return nil, errors.New("qoderwasm: 缺少加密推理 host")
	}

	mod, err := New(ctx)
	if err != nil {
		return nil, err
	}
	s := &Signer{mod: mod, host: strings.TrimPrefix(opts.Host, "https://"), opts: opts}
	if err := s.init(ctx, opts); err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	return s, nil
}

// Close 释放 WASM 资源。
func (s *Signer) Close(ctx context.Context) error {
	if s == nil || s.mod == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != 0 {
		// 释放 context（导出的是 __wbg_qodercontext_free）。
		if f := s.mod.mod.ExportedFunction("__wbg_qodercontext_free"); f != nil {
			_, _ = f.Call(ctx, 0, uint64(s.ctx))
		}
		s.ctx = 0
	}
	return s.mod.Close(ctx)
}

// init 生成鉴权字段并建 QoderContext。
func (s *Signer) init(ctx context.Context, opts SignerOptions) error {
	// ---- 1. 鉴权字段（服务端据此识别用户身份）----
	userPayload, err := json.Marshal(map[string]any{
		"uid":                  opts.UID,
		"security_oauth_token": opts.SecurityOAuthToken,
		"organization_id":      opts.OrganizationID,
		"organization_tags":    orEmptySlice(opts.OrganizationTags),
		"data_policy_agreed":   opts.DataPolicyAgreed,
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
		return fmt.Errorf("qoderwasm: 鉴权字段为空（uid=%s）—— WASM 没算出身份", opts.UID)
	}

	// ---- 2. QoderContext ----
	userInfo, err := json.Marshal(map[string]any{
		"uid":               opts.UID,
		"encrypt_user_info": fields.EncryptUserInfo,
		"key":               fields.Key,
		"organization_id":   opts.OrganizationID,
		"organization_tags": orEmptySlice(opts.OrganizationTags),
		"data_policy_agreed": opts.DataPolicyAgreed,
	})
	if err != nil {
		return err
	}
	version := opts.ClientVersion
	if version == "" {
		version = defaultClientVersion
	}
	meta := map[string]string{
		"client_type":      orDefault(opts.ClientType, "5"),
		"business_product": orDefault(opts.BusinessProduct, "cli"),
		"business_type":    orDefault(opts.BusinessType, "agent"),
		"scene":            orDefault(opts.Scene, "assistant"),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	s.meta = string(metaJSON)

	// 参数顺序：(sp, machineId, len, version, len, userInfo, len, clientMeta, len)
	ctxHandle, err := s.mod.callPointerWithStrings(s.mod.fnCtxNew,
		opts.MachineID, version, string(userInfo), s.meta)
	if err != nil {
		return fmt.Errorf("qoderwasm: 建 QoderContext 失败: %w", err)
	}
	if ctxHandle == 0 {
		return errors.New("qoderwasm: QoderContext 句柄为空")
	}
	s.ctx = ctxHandle
	return nil
}

// BuildInferRequest 实现 qoder.RequestSigner。
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
func (s *Signer) BuildInferRequest(model string, body []byte) (string, []byte, map[string]string, error) {
	if s == nil || s.mod == nil || s.ctx == 0 {
		return "", nil, nil, errors.New("qoderwasm: 签名器未初始化")
	}
	ask, err := parseOpenAIBody(model, body)
	if err != nil {
		return "", nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 内存超限就换一个干净实例（这个 WASM 靠宿主 GC 回收，我们只能定期重建）。
	// 见 memLimitBytes 的注释与实测曲线。
	s.calls++
	s.recycleLocked()

	payload, err := buildPayload(ask)
	if err != nil {
		return "", nil, nil, err
	}

	// 参数顺序：
	// (sp, context, host, hostLen, body, bodyLen, modelKey, keyLen, source, srcLen)
	//
	// ⚠ **不归还**这四个字符串（与参照实现一致）。
	//
	// 我第一版在这里 `defer freeString(...)` —— 结果**第二次**调用就
	// `分配 6 字节失败: out of bounds memory access`。
	// 根因：`prepareInferRequest` 会把 host / body / key / source 的指针
	// **留在 result 里**，归还那块内存等于让 result 指向已回收的空间。
	//
	// ⚠ **也不能复用缓冲**（我试过，同样是错的）。
	//
	// 一开始我想"分配一次、之后原地改写"来避免内存增长，实测**第一次
	// 调用就 `unreachable`**：四个串分别占 1117712 / 1121816 / 1125920 /
	// 1130024，彼此相距 4096（我给的块大小）—— 说明 WASM 在
	// prepareInferRequest 里对**每个入参**都做了某种按块边界的处理
	//（大概率是把指针当"独立分配"来标记/释放）。共享块会让它算错边界。
	//
	// 所以只能每次新分配。代价是 WASM 内存随调用次数增长，但那是
	// **这个 WASM 的固有行为**，不是我们漏归还 —— 参照实现（JS）也是
	// 每次新分配，只是靠 GC 回收。见 TestMemoryGrowthIsBounded 的实测。
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
		uint64(s.ctx),
		uint64(hostPtr), uint64(hostLen),
		uint64(bodyPtr), uint64(bodyLen),
		uint64(keyPtr), uint64(keyLen),
		uint64(srcPtr), uint64(srcLen))
	if err != nil {
		return "", nil, nil, fmt.Errorf("qoderwasm: 构造加密请求失败: %w", err)
	}

	headers, err := s.headersOf(result)
	if err != nil {
		return "", nil, nil, err
	}

	// ⚠ 参数顺序：(栈指针, result) —— 与直觉相反（栈指针在前）。
	// ⚠ 用 callResultString 而不是 callString：返回值布局不同（无错误槽）。
	url, err := s.mod.callResultString(s.mod.fnURL, result)
	if err != nil {
		return "", nil, nil, fmt.Errorf("qoderwasm: 取 URL 失败: %w", err)
	}
	encBody, err := s.mod.callResultString(s.mod.fnBody, result)
	if err != nil {
		return "", nil, nil, fmt.Errorf("qoderwasm: 取请求体失败: %w", err)
	}

	// result 读完即释放 —— 它同时也是"入参字符串不再被引用"的信号，
	// 所以必须在读完 url/body 之后、返回之前做。
	s.releaseResult(result)

	// URL 的 query 部分由调用方拼到加密端点上。
	path := url
	if i := strings.IndexByte(url, '?'); i >= 0 {
		path = url[i:]
	} else {
		path = ""
	}
	if !strings.Contains(url, "/algo/api/v2/service/pro/sse/agent_chat_generation") {
		return "", nil, nil, fmt.Errorf("qoderwasm: WASM 给出的 URL 不是加密推理端点（%.120q）—— "+
			"host 或模型 key 不对", url)
	}
	return path, []byte(encBody), headers, nil
}

// releaseResult 释放一个 requestresult。
//
// # 为什么可以释放（实测）
//
// 与入参字符串不同，result 只在本次调用内被读（headers / url / body），
// 读完就没有别的引用了。实测：释放后连续 5 次调用全部成功
//（不释放也是 5/5，但内存每次涨约 5 KB）。
//
// ⚠ 参数顺序：参照实现从不调这个函数（它靠 JS GC），所以没有可对照的
// 调用点。实测两种顺序（`(result, 0)` 与 `(0, result)`）都能跑通 ——
// 说明其中一个参数是"对齐/尺寸"类的次要参数。这里用 `(0, result)`，
// 与 `__wbg_qodercontext_free` 的写法保持一致。
func (s *Signer) releaseResult(result uint32) {
	if result == 0 {
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
// **对象堆索引**。把 result 当对象索引去查会越界（我第一次探针就 panic 在这）。
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
