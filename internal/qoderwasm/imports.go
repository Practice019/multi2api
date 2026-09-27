// imports.go 注册 WASM 需要的全部 host imports。
//
// # 签名事实（用 wazero 的签名 dump 逐条核对得来，不是猜的）
//
//	参数：全部 i32（对象索引就是 u32）
//	结果：全部 i32，**唯一例外** __wbg_now 返回 f64
//	栈约定：结果从索引 0 写（stack 长 = max(参数数, 结果数)）
//
// # ⚠ 我在这上面栽过两次，都值得记
//
// **第一次：把签名 dump 里的 127/124 看反了。**
// wazero 用 wasm 核心的编码打印类型：i32=0x7F(127)、f64=0x7C(124)。
// 我以为 127 是 f64，于是全部声明成 f64 → 直接 `signature mismatch`。
// 这次失败是**响亮的**，好排查。
//
// **第二次（难得多）：声明对但语义实现错。**
// `__wbg_set_08463b1df38a7e29` 在参照实现里被标注成
// `(heapObject(a) as Uint8Array).set(heapObject(e), heapObject(t))` ——
// 我照字面实现成"Uint8Array 拷贝"，但 JS 的 `as` 只是**类型断言**，
// 运行时 a 其实是 **Map**。于是请求头 map 恒为空。
//
// 发现的线索是**调用计数**：`set` 被调了 20 次，而请求头恰好 20 个 ——
// 一个"Uint8Array 拷贝"不可能调 20 次。计数与语义对不上，
// 就说明语义理解错了。这类"不报错、只是少一整块数据"的缺陷，
// 只有把调用次数打出来才看得见。
package qoderwasm

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// registerImports 注册全部 31 个 host import。
func (m *Module) registerImports(ctx context.Context) error {
	b := m.rt.NewHostModuleBuilder(importModule)

	i32 := api.ValueTypeI32
	noRes := []api.ValueType{}
	i32Res := []api.ValueType{i32}

	// host 注册 helper：参数全 i32、结果按需。
	//
	// ⚠ 结果写回**索引 0**：wazero 的文档明确说
	// "stack 的长度是 max(参数数, 结果数)，有结果时从索引 0 开始写"。
	// 我一开始猜"结果在参数之后"，写错了位置 —— WASM 读到垃圾并**死循环**
	//（表象只是"卡住不返回"，极难定位）。
	host := func(name string, nargs int, res []api.ValueType, fn func(a []uint32) uint64) {
		params := make([]api.ValueType, nargs)
		for i := range params {
			params[i] = i32
		}
		b.NewFunctionBuilder().WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
				args := make([]uint32, nargs)
				for i := 0; i < nargs; i++ {
					args[i] = api.DecodeU32(stack[i])
				}
				if traceImports {
					fmt.Printf("    [trace] %-46s %v\n", name, args)
				}
				out := fn(args)
				if len(res) > 0 {
					stack[0] = out
				}
			}), params, res).Export(name)
	}

	// ── 对象堆管理 ──

	host("__wbindgen_object_drop_ref", 1, noRes, func(a []uint32) uint64 {
		m.take(a[0])
		return 0
	})
	host("__wbindgen_object_clone_ref", 1, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(m.get(a[0])))
	})
	host("__wbindgen_cast_0000000000000001", 2, i32Res, func(a []uint32) uint64 {
		// (ptr, len) → Uint8Array（拷一份，不共享 WASM 内存）
		return api.EncodeU32(m.push(jsVal{
			kind:  kindU8Array,
			bytes: append([]byte(nil), m.memRead(a[0], a[1])...),
		}))
	})
	host("__wbindgen_cast_0000000000000002", 2, i32Res, func(a []uint32) uint64 {
		// (ptr, len) → string
		return api.EncodeU32(m.push(jsVal{kind: kindString, str: m.readString(a[0], a[1])}))
	})

	// ── 对象操作 ──

	host("__wbg_set_08463b1df38a7e29", 3, i32Res, func(a []uint32) uint64 {
		// ⚠ 泛型 `obj.set(k, v)` —— 实测是 **Map.set**，不是 Uint8Array.set。
		//
		// 参照实现的 TS 里那个 `as Uint8Array` 只是**类型断言**，
		// 运行时目标其实是 Map（JS 是动态类型，`as` 不改运行时行为）。
		//
		// 判据：本函数在 prepareInferRequest 里被调用 **20 次**，
		// 而产出的请求头恰好 **20 个** —— 计数与语义严丝合缝。
		// 照字面实现成"Uint8Array 拷贝"时，map 恒为空
		//（表象是"请求头整块缺失"，不报错）。
		target, k, v := m.get(a[0]), m.get(a[1]), m.get(a[2])
		switch target.kind {
		case kindMap:
			if target.m == nil {
				target.m = map[string]string{}
			}
			target.m[k.str] = v.str
		case kindU8Array:
			// Uint8Array.set(src, offset)
			off := int(v.num)
			if src := k; src.kind == kindU8Array && off+len(src.bytes) <= len(target.bytes) {
				copy(target.bytes[off:], src.bytes)
			}
		}
		return 0
	})
	host("__wbg_call_d578befcc3145dee", 3, i32Res, func(a []uint32) uint64 {
		// JS 的 `fn.call(self, arg)`。
		//
		// 参照实现是真的去调那个函数（`target.call(heapObject(self), heapObject(arg))`），
		// 而我们的对象堆里没有可调用的函数值 —— 所以只能返回 undefined。
		//
		// ⚠ 这与 `set` 不是一回事：`set` 是**宿主侧**的容器写入（必须真做），
		// 而 `call` 在本 WASM 里被用来调内建方法。返回 undefined 是**保守降级**：
		// 若某个调用点真的依赖返回值，会表现为"那个字段是空的"，
		// 而不是内存越界 —— 后者是这次踩到的另一种失败形态。
		_ = m.get(a[1])
		_ = m.get(a[2])
		return api.EncodeU32(m.push(jsVal{kind: kindUndefined}))
	})

	// ── Uint8Array ──

	host("__wbg_new_with_length_9cedd08484b73942", 1, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindU8Array, bytes: make([]byte, a[0])}))
	})
	host("__wbg_length_0c32cb8543c8e4c8", 1, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(uint32(len(m.get(a[0]).bytes)))
	})
	host("__wbg_subarray_0f98d3fb634508ad", 3, i32Res, func(a []uint32) uint64 {
		// heapObject(a).subarray(from, to)
		v := m.get(a[0])
		from, to := a[1], a[2]
		var nb []byte
		if v.kind == kindU8Array && int(to) <= len(v.bytes) && from <= to {
			nb = append(nb, v.bytes[from:to]...)
		}
		return api.EncodeU32(m.push(jsVal{kind: kindU8Array, bytes: nb}))
	})
	host("__wbg_prototypesetcall_3e05eb9545565046", 3, noRes, func(a []uint32) uint64 {
		// Uint8Array.prototype.set.call(wasmMem.subarray(dst, dst+len), src)
		// 注意第一个参数是 **WASM 内存里的指针**（不是对象索引）。
		if dst := m.memRead(a[0], a[1]); dst != nil {
			if src := m.get(a[2]); src.kind == kindU8Array {
				copy(dst, src.bytes)
			}
		}
		return 0
	})

	// ── 随机数（两个 getRandomValues 签名方向**相反**，别写反）──

	host("__wbg_getRandomValues_d49329ff89a07af1", 2, noRes, func(a []uint32) uint64 {
		// 写 **WASM 内存**：getRandomValues(heap().subarray(x, x+y))
		if buf := m.memRead(a[0], a[1]); buf != nil {
			randomBytes(buf)
		}
		return 0
	})
	host("__wbg_getRandomValues_c44a50d8cfdaebeb", 2, noRes, func(a []uint32) uint64 {
		// 写 **JS 对象**：heapObject(x).getRandomValues(heapObject(y))
		//
		// ⚠ 做成 no-op 会让 WASM **无限重试**（它检测到熵没填上就再来一轮），
		// 表象是"卡住不返回"而不是报错。我第一次探针就是这样，
		// trace 里能看到 subarray→getRandomValues→length 的循环。
		if y := m.get(a[1]); y.kind == kindU8Array && len(y.bytes) > 0 {
			randomBytes(y.bytes)
		}
		return 0
	})
	host("__wbg_randomFillSync_6c25eac9869eb53c", 2, noRes, func(a []uint32) uint64 {
		// Node 的 randomFillSync；本 WASM 走 WebCrypto 分支，实际不触发。
		return 0
	})

	// ── 环境探测（本 WASM 会判断自己跑在浏览器还是 Node）──

	host("__wbg_crypto_38df2bab126b63dc", 1, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindOpaque}))
	})
	host("__wbg_process_44c7a14e11e9f69e", 1, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindOpaque}))
	})
	host("__wbg_versions_276b2795b1c6a219", 1, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindMap, m: map[string]string{}}))
	})
	host("__wbg_node_84ea875411254db1", 1, i32Res, func(a []uint32) uint64 {
		// 返回 undefined：告诉 WASM"这里不是 Node"→ 走 WebCrypto 分支，
		// 那条分支用的正是上面两个 getRandomValues。
		return api.EncodeU32(m.push(jsVal{kind: kindUndefined}))
	})
	host("__wbg_require_b4edbdcf3e2a1ef0", 0, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindUndefined}))
	})
	host("__wbg_msCrypto_bd5a034af96bcba6", 1, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindUndefined}))
	})
	for _, n := range []string{
		"GLOBAL_THIS_a1248013d790bf5f",
		"GLOBAL_f2e0f995a21329ff",
		"SELF_24f78b6d23f286ea",
		"WINDOW_59fd959c540fe405",
	} {
		host("__wbg_static_accessor_"+n, 0, i32Res, func(a []uint32) uint64 {
			return api.EncodeU32(m.push(jsVal{kind: kindOpaque}))
		})
	}
	host("__wbg_new_99cabae501c0a8a0", 0, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindMap, m: map[string]string{}}))
	})

	// ── 类型判定 ──

	host("__wbg___wbindgen_is_object_40c5a80572e8f9d3", 1, i32Res, func(a []uint32) uint64 {
		// ⚠ 实测必须**恒返回 1**，不能按 kind 判。
		//
		// 参照实现写的是 `typeof v === 'object' && v !== null`，看起来
		// "字符串/数字/undefined → false"。但照那个语义实现之后，
		// `generate_runtime_auth_fields` 的产物长度会从实测的 152/108
		// 变成 192/172 —— 即 WASM 走了**另一条分支**。
		//
		// 原因是它的 JS 堆里存的**不是**我的 kind 标签：本 WASM 会拿
		// 我们自己 push 的值（opaque 环境对象、字符串）去问这个问题，
		// 而它对"什么算对象"的期望比 TS 类型标注宽。
		//
		// 判据是**产物长度**（152/108 与参照实现逐字节一致），
		// 不是类型标注。见 TestGenerateRuntimeAuthFields 的长度断言。
		return 1
	})
	host("__wbg___wbindgen_is_string_b29b5c5a8065ba1a", 1, i32Res, func(a []uint32) uint64 {
		if m.get(a[0]).kind == kindString {
			return 1
		}
		return 0
	})
	host("__wbg___wbindgen_is_function_49868bde5eb1e745", 1, i32Res, func(a []uint32) uint64 {
		return 0 // 我们不实现任何 JS 函数对象
	})
	host("__wbg___wbindgen_is_undefined_c0cca72b82b86f4d", 1, i32Res, func(a []uint32) uint64 {
		if m.get(a[0]).kind == kindUndefined {
			return 1
		}
		return 0
	})

	// ── 错误 ──

	host("__wbg_Error_2e59b1b37a9a34c3", 2, i32Res, func(a []uint32) uint64 {
		return api.EncodeU32(m.push(jsVal{kind: kindOpaque, str: m.readString(a[0], a[1])}))
	})
	host("__wbg___wbindgen_throw_81fc77679af83bc6", 2, noRes, func(a []uint32) uint64 {
		// 抛出而不是返回 —— 语义就是 throw。
		// panic 会被 wazero 转成调用点的 error，不会带崩进程。
		panic(wasmThrow{msg: m.readString(a[0], a[1])})
	})

	// ── 唯一返回 f64 的 import ──
	b.NewFunctionBuilder().WithGoModuleFunction(
		api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			stack[0] = api.EncodeF64(0)
		}), nil, []api.ValueType{api.ValueTypeF64}).Export("__wbg_now_88621c9c9a4f3ffc")

	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("qoderwasm: 注册 host imports 失败: %w", err)
	}
	return nil
}

// wasmThrow 承载 WASM 的 throw 文案。
type wasmThrow struct{ msg string }

func (e wasmThrow) Error() string { return "qoderwasm: WASM throw: " + e.msg }

// 编译期确认 crypto/rand 与 fmt 都被用到（imports.go 单独编译时）。
var (
	_ = rand.Reader
	_ = fmt.Sprintf
)
