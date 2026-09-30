// Package qoderwasm 把 Qoder 客户端内嵌的 WASM（wasm-bindgen 产物）接到 Go。
//
// # 为什么需要它
//
// Qoder 客户端的**真实**推理不走公开的 OpenAI 兼容端点，而是走**加密端点**：
//
//	POST {host}/algo/api/v2/service/pro/sse/agent_chat_generation
//	     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1
//	body: <由本 WASM 加密>
//
// 服务端认的是**模型目录里的 key**（`qfmodel` / `dmodel` / …），
// 而公开端点只认一小撮通用名。所以没有这块，Qoder 的对话能力等于不可用。
//
// # ⚠ 不是"破解密码学"
//
// WASM 自己导出了 `decrypt_server_response` / `model_cache_decrypt` 等
// 成对的编解码函数。我们**直接调用它**，不逆向其算法 ——
// 等同"用客户端自己的钥匙开自己的锁"。
//
// # 为什么单独一个包（而不是塞进 internal/qoder）
//
// 三个理由，每个都独立成立：
//
//  1. **依赖隔离**：本包是唯一引入 wazero（唯一新增第三方依赖）的地方。
//     塞进 internal/qoder 会让那个包也背上 wazero。
//  2. **架构判据**：本包**不依赖 gateway**，因此不会被 arch_test 的
//     discoverUpstreams 认成"上游实现"。它是 qoder 的一个**组件**，
//     由装配层注入（qoder.RequestSigner 那个接缝）。
//  3. **可独立测试**：WASM 桥的行为可以用固定输入直接验证，
//     不需要起假上游。
//
// # ⚠ 本包不在 internal/qoder 里 —— 别在这里 import gateway
//
// 一旦 import 了 gateway，arch_test 的 discoverUpstreams 会把它当成一个
// 上游实现，而它没有 Provider 方法 → 判据会红。这不是巧合：
// "消费契约 ⟺ 是上游" 正是那条判据的定义，而本包**消费的不是契约**，
// 是 WASM 的 ABI。
package qoderwasm

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// wasmBytes WASM 模块本体（298 KB，wasm-bindgen 产物）。
//
// ⚠ 用 `//go:embed` 而不是运行时读文件：这样二进制自带 WASM，
// 部署时不会出现"忘拷 wasm 文件 → 加密推理静默不可用"。
// 代价是二进制大 298 KB —— 与"少一个可静默失效的部署步骤"相比很值。
//
//go:embed qoder_auth_wasm.wasm
var wasmBytes []byte

// importModule WASM 内嵌的 import 模块名。
//
// ⚠ 名字必须**逐字节一致**，否则实例化会因缺 import 而失败。
// 这个串 embed 在 wasm 里，改不了 —— 只能照抄。
const importModule = "./qoder_auth_wasm_bg.js"

// ErrClosed 模块已关闭。
var ErrClosed = errors.New("qoderwasm: 模块已关闭")

// jsVal 模拟 wasm-bindgen 的 JS 对象堆里的一个值。
//
// # 为什么需要"对象堆"
//
// wasm-bindgen 把 JS 的引用类型（对象/数组/Map）编码成整数索引，
// WASM 侧只传这些索引。所以宿主必须自己维护一张**索引 → 值**的表，
// 否则 WASM 传回来的索引无处可查。
//
// 参照实现用 `unknown[]`，这里用带 kind 标签的结构体：Go 是静态类型，
// 需要显式区分"这是个 Uint8Array"还是"这是个 Map"。
type jsVal struct {
	// kind 值的类别。
	kind jsKind
	// bytes u8array 的字节。
	bytes []byte
	// num number 的值。
	num float64
	// str string 的值。
	str string
	// m map 的键值对。
	m map[string]string
}

// jsKind JS 值的类别。
type jsKind uint8

const (
	kindUndefined jsKind = iota
	kindNumber
	kindString
	kindU8Array
	kindMap
	// kindOpaque 本实现不解释其内容的 JS 对象（crypto / process / global …）。
	//
	// 它们的存在只为满足"is_undefined 判 false"这类检查 ——
	// 参照实现里也是真对象，我们给一个"非 undefined 的不透明值"即可。
	kindOpaque
)

// sentinelCount 对象堆前置哨兵数量（与官方 `wW` 一致）。
//
// 官方做法：1024 个 undefined，再 push 四个哨兵
// （undefined / null / true / false）。所以**前 1028 个索引不可回收** ——
// take 里据此判断。抄这个布局而不是自己发明，是因为 WASM 侧对
// 特定索引可能有内建期望（官方生成器就是这么写的）。
const sentinelCount = 1028

// traceImports 调试用：打开后打印每次 host import 调用。
//
// 生产里恒为 false —— 它是排查"WASM 死循环/越界"这类**无错误信息**故障
// 的唯一手段（我的探针就是靠它发现 getRandomValues 做成 no-op 会导致
// 无限重试）。
var traceImports = false

// Module 一个已实例化的 WASM 模块。
//
// # 并发
//
// 实例化后的模块本身无状态，但**对象堆是共享可变状态** ——
// 所以本类型用一把锁串行化所有调用。
//
// 为什么不给每个调用者一个独立实例：实例化一次要编译 298 KB 的 WASM
// （实测数百毫秒），而每次推理都要用。串行化的代价是"同一时刻只能构造
// 一个加密请求"，而那个操作本身只有微秒级 —— 不构成瓶颈。
type Module struct {
	rt  wazero.Runtime
	mod api.Module

	mu   sync.Mutex
	obj  []jsVal
	free int

	closed bool

	// 导出函数缓存（避免每次调用都查表）。
	fnAlloc     api.Function
	fnStackAdj  api.Function
	fnFree      api.Function
	fnGenFields api.Function
	fnCtxNew    api.Function
	fnPrepare   api.Function
	fnHdr       api.Function
	fnURL       api.Function
	fnBody      api.Function
}

// New 编译并实例化 WASM 模块。
//
// ctx 只用于实例化阶段（编译可能耗时数百毫秒）。
func New(ctx context.Context) (*Module, error) {
	rt := wazero.NewRuntime(ctx)

	m := &Module{rt: rt}
	if err := m.instantiate(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	return m, nil
}

// Close 释放运行时。
//
// 幂等：重复调用返回 nil（与 io.Closer 的惯例一致，且避免
// defer Close 与显式 Close 同时存在时二次报错）。
func (m *Module) Close(ctx context.Context) error {
	if m == nil || m.closed {
		return nil
	}
	m.closed = true
	return m.rt.Close(ctx)
}

// instantiate 注册 host imports 并实例化模块。
func (m *Module) instantiate(ctx context.Context) error {
	m.obj = make([]jsVal, 1024)
	for i := range m.obj {
		m.obj[i] = jsVal{kind: kindUndefined}
	}
	// 四个哨兵：undefined / null / true / false
	m.obj = append(m.obj,
		jsVal{kind: kindUndefined},
		jsVal{kind: kindUndefined},
		jsVal{kind: kindNumber, num: 1},
		jsVal{kind: kindNumber, num: 0},
	)
	m.free = len(m.obj)

	if err := m.registerImports(ctx); err != nil {
		return err
	}
	mod, err := m.rt.Instantiate(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("qoderwasm: 实例化 WASM 失败: %w", err)
	}
	m.mod = mod

	// 缓存导出函数。缺任何一个都说明 WASM 版本变了 —— 明确报错，
	// 不要等到第一次推理时才 nil panic。
	for _, spec := range []struct {
		name string
		dst  *api.Function
	}{
		{"__wbindgen_export2", &m.fnAlloc},
		{"__wbindgen_add_to_stack_pointer", &m.fnStackAdj},
		{"__wbindgen_export4", &m.fnFree},
		{"generate_runtime_auth_fields", &m.fnGenFields},
		{"qodercontext_new", &m.fnCtxNew},
		{"qodercontext_prepareInferRequest", &m.fnPrepare},
		{"requestresult_headers", &m.fnHdr},
		{"requestresult_url", &m.fnURL},
		{"requestresult_body", &m.fnBody},
	} {
		f := mod.ExportedFunction(spec.name)
		if f == nil {
			return fmt.Errorf("qoderwasm: WASM 缺少导出 %q —— "+
				"wasm 文件与桥的实现不匹配（版本变了？）", spec.name)
		}
		*spec.dst = f
	}
	return nil
}

// ── 对象堆 ──────────────────────────────────────────────────────────────

// push 把一个值放进对象堆，返回其索引。
func (m *Module) push(v jsVal) uint32 {
	if m.free == len(m.obj) {
		// 堆满了：按官方做法把"下一个空闲"写成自增索引。
		m.obj = append(m.obj, jsVal{kind: kindNumber, num: float64(len(m.obj) + 1)})
	}
	i := m.free
	m.free = int(m.obj[i].num)
	m.obj[i] = v
	return uint32(i)
}

// take 取出并回收一个索引。
//
// 前 sentinelCount 个索引是哨兵，**不可回收**（与官方一致）。
func (m *Module) take(i uint32) jsVal {
	v := m.obj[i]
	if i >= sentinelCount {
		m.obj[i] = jsVal{kind: kindNumber, num: float64(m.free)}
		m.free = int(i)
	}
	return v
}

// get 读一个索引但不回收。
func (m *Module) get(i uint32) jsVal {
	if int(i) >= len(m.obj) {
		// 越界说明 WASM 给了个我们没 push 过的索引 —— 通常是某个 import
		// 返回值写错了位置。返回 undefined 而不是 panic：
		// panic 会带崩整个进程，而"值不对"最多让这次推理失败。
		return jsVal{kind: kindUndefined}
	}
	return m.obj[i]
}

// ── 内存与字符串 ────────────────────────────────────────────────────────

// memRead 读 WASM 线性内存。
func (m *Module) memRead(ptr, n uint32) []byte {
	b, ok := m.mod.Memory().Read(ptr, n)
	if !ok {
		return nil
	}
	return b
}

// readString 读 WASM 内存里的 UTF-8 字符串。
func (m *Module) readString(ptr, n uint32) string {
	return string(m.memRead(ptr, n))
}

// writeString 把字符串写进 WASM 内存，返回 (指针, 长度)。
//
// 走 WASM 自己的分配器（`__wbindgen_export2`）而不是随便找个地方写：
// 那块内存归 WASM 的分配器管，写进去的内容它才会在需要时读。
func (m *Module) writeString(s string) (uint32, uint32, error) {
	res, err := m.fnAlloc.Call(context.Background(), uint64(len(s)), 1)
	if err != nil {
		return 0, 0, fmt.Errorf("qoderwasm: 分配 %d 字节失败: %w", len(s), err)
	}
	ptr := uint32(res[0])
	if ptr == 0 && len(s) > 0 {
		return 0, 0, errors.New("qoderwasm: WASM 分配器返回空指针")
	}
	buf := m.memRead(ptr, uint32(len(s)))
	if buf == nil {
		return 0, 0, fmt.Errorf("qoderwasm: 分配到的区间 [%d,%d) 不可写", ptr, ptr+uint32(len(s)))
	}
	copy(buf, s)
	return ptr, uint32(len(s)), nil
}

// ── 栈辅助 ──────────────────────────────────────────────────────────────

// pushStack 在 WASM 栈上开 16 字节的返回区。
func (m *Module) pushStack() (uint32, error) {
	// JS 传 -16（i32 补码），Go 里要显式构造同样的位模式。
	res, err := m.fnStackAdj.Call(context.Background(), uint64(^uint32(15)))
	if err != nil {
		return 0, fmt.Errorf("qoderwasm: 调整栈指针失败: %w", err)
	}
	return uint32(res[0]), nil
}

// popStack 归还 16 字节返回区。
func (m *Module) popStack() {
	_, _ = m.fnStackAdj.Call(context.Background(), 16)
}

// callString 调一个"写字符串到栈"的导出函数，返回其字符串。
//
// # 返回值布局（与 callPointer 不同，别混用）
//
//	字符串类：ptr(0) / len(4) / valIdx(8) / isErr(12)
//	指针类  ：ptr(0) / errIdx(4) / isErr(8)
//
// ⚠ 混用会得到 `null pointer passed to rust`。参照实现把这条
// 写进了文件头注释（它踩过），这里照抄并保留。
func (m *Module) callString(fn api.Function, args ...uint64) (string, error) {
	stack, err := m.pushStack()
	if err != nil {
		return "", err
	}
	defer m.popStack()

	callArgs := append([]uint64{uint64(stack)}, args...)
	if _, err := fn.Call(context.Background(), callArgs...); err != nil {
		return "", fmt.Errorf("qoderwasm: WASM 调用失败: %w", err)
	}
	mem := m.memRead(stack, 16)
	if mem == nil {
		return "", errors.New("qoderwasm: 无法读返回区")
	}
	ptr := le32(mem[0:])
	n := le32(mem[4:])
	valIdx := le32(mem[8:])
	isErr := le32(mem[12:])
	if isErr != 0 {
		return "", m.wasmError(valIdx)
	}
	if ptr == 0 {
		return "", nil
	}
	return m.readString(ptr, n), nil
}

// callPointer 调一个"写指针到栈"的导出函数，返回其指针。
//
// 布局见 callString 的注释（两者**不同**）。
func (m *Module) callPointer(fn api.Function, args ...uint64) (uint32, error) {
	stack, err := m.pushStack()
	if err != nil {
		return 0, err
	}
	defer m.popStack()

	callArgs := append([]uint64{uint64(stack)}, args...)
	if _, err := fn.Call(context.Background(), callArgs...); err != nil {
		return 0, fmt.Errorf("qoderwasm: WASM 调用失败: %w", err)
	}
	mem := m.memRead(stack, 16)
	if mem == nil {
		return 0, errors.New("qoderwasm: 无法读返回区")
	}
	ptr := le32(mem[0:])
	errIdx := le32(mem[4:])
	isErr := le32(mem[8:])
	if isErr != 0 {
		return 0, m.wasmError(errIdx)
	}
	return ptr, nil
}

// callStringWithStrings 调"多段字符串入参、返回字符串"的导出函数。
//
// 入参按 (ptr, len) 成对铺开，函数自己会写进栈前的 16 字节返回区。
//
// ⚠ 同样**不归还**入参：`generate_runtime_auth_fields` 的产物会被后续的
// `qodercontext_new` 用到，而"什么时候能还"只有被调函数知道。
// 见 callPointerWithStrings 的注释。
func (m *Module) callStringWithStrings(fn api.Function, parts ...string) (string, error) {
	args, err := m.writeParts(parts)
	if err != nil {
		return "", err
	}
	return m.callString(fn, args...)
}

// callPointerWithStrings 调"多段字符串入参、返回指针"的导出函数。
//
// ⚠ **不归还**入参字符串（与参照实现一致）。
//
// # 为什么不能还（实测踩到的真实缺陷）
//
// 我第一版在这里 `defer cleanup()` 归还 —— 于是 `qodercontext_new`
// 之后的**每一次** prepareInferRequest 都 `out of bounds memory access`，
// 且 trace 显示崩溃前**一个 import 都没调**（说明 WASM 一进去就踩空了）。
//
// 原因：`QoderContext` 会**保留**指向这些字符串的指针（它把 machineId /
// version / userInfo / meta 存进自己的结构，而不是立刻拷走）。
// 归还那块内存后，context 里就留下一串悬垂指针。
//
// 参照实现根本不归还（JS 侧靠 GC，只要对象还被引用就不会回收）——
// 也就是说"归还"这件事在 JS 里是**自动且时机正确**的，
// 手写成"调用返回就还"是错的。
//
// 代价：这些串在 context 生命周期内一直占着 WASM 内存。
// 但它们的总长只有几百字节，而一个 Signer 只建一次 context —— 可以接受。
func (m *Module) callPointerWithStrings(fn api.Function, parts ...string) (uint32, error) {
	args, err := m.writeParts(parts)
	if err != nil {
		return 0, err
	}
	return m.callPointer(fn, args...)
}

// writeParts 把多段字符串写进 WASM 内存，返回参数列表。
//
// ⚠ 不返回"归还函数"：见 callPointerWithStrings 的注释 ——
// 调用方拿到归还函数就一定会用，而"什么时候能还"是**被调函数**才知道的事。
// 与其给一个危险的便利，不如不给。
func (m *Module) writeParts(parts []string) ([]uint64, error) {
	args := make([]uint64, 0, len(parts)*2)
	for _, p := range parts {
		ptr, n, err := m.writeString(p)
		if err != nil {
			return nil, err
		}
		args = append(args, uint64(ptr), uint64(n))
	}
	return args, nil
}

// callResultString 调 `requestresult_url` / `requestresult_body` 这类
// "取 result 的某个字符串字段"的函数。
//
// # ⚠ 它与 callString 的**返回值布局不同**（虽然入参形状一样）
//
//	callString         ptr(0) / len(4) / valIdx(8) / isErr(12)   ← 有错误槽
//	callResultString   ptr(0) / len(4)                           ← **没有**错误槽
//
// 参照实现就是这么读的（它只取 ptr 与 len，不做错误判定）。
//
// 我第一版图省事直接复用了 callString —— 于是它去读偏移 12 的 isErr，
// 而那 16 字节栈区**没人写过**，里面是上一次调用的残留垃圾，
// 于是被误判成"WASM 报错"，报出一句没有文案的错误
// （`WASM 未提供错误文案` —— 因为那个偏移也不真是 valIdx）。
//
// 教训：**"入参形状相同"不等于"返回值布局相同"**。
// 参照实现把"字符串类"和"指针类"两种布局写进了文件头注释，
// 而这两种之下还藏着第三种（无错误槽）—— 只有照它逐行读才看得出。
func (m *Module) callResultString(fn api.Function, result uint32) (string, error) {
	stack, err := m.pushStack()
	if err != nil {
		return "", err
	}
	defer m.popStack()

	// ⚠ 参数顺序：(栈指针, result) —— 与直觉相反（栈指针在前）。
	if _, err := fn.Call(context.Background(), uint64(stack), uint64(result)); err != nil {
		return "", fmt.Errorf("qoderwasm: WASM 调用失败: %w", err)
	}
	mem := m.memRead(stack, 16)
	if mem == nil {
		return "", errors.New("qoderwasm: 无法读返回区")
	}
	ptr := le32(mem[0:])
	n := le32(mem[4:])
	if ptr == 0 {
		return "", nil
	}
	return m.readString(ptr, n), nil
}

// wasmError 把对象堆里的异常值转成 Go error。
func (m *Module) wasmError(idx uint32) error {
	v := m.take(idx)
	msg := v.str
	if msg == "" {
		msg = "（WASM 未提供错误文案）"
	}
	return fmt.Errorf("qoderwasm: WASM 报错: %s", msg)
}

// le32 从小端字节里读 uint32。
func le32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// randomBytes 填随机字节。
//
// 用 crypto/rand 而不是 math/rand：WASM 拿它生成密钥与 nonce，
// 可预测的随机数会让加密形同虚设。
func randomBytes(b []byte) {
	if len(b) == 0 {
		return
	}
	_, _ = rand.Read(b)
}
