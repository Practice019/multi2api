// errorclassifier_test.go —— 第七个扩展点（gateway.ErrorClassifier）的判据锁定。
//
// # 本文件钉住什么
//
// 三件事，每件都对应 P2 缺口的一个具体后果：
//
//  1. `gateway.ErrorKind` 的中立取值与两个上游的 ErrKind **逐项对齐**
//     （防枚举漂移，那是静默错位类 bug 的温床）。
//  2. `DefaultErrorKind` 的兜底**不含任何上游专有词汇**（正文一律不看）。
//  3. `ErrorClassifier` 是**可选**扩展点：不实现不算错（ExtOf 返回 false）。
package gateway

import (
	"testing"
)

// TestErrorKindValuesAreStable 锁定 ErrorKind 的取值。
//
// # 为什么必须钉住数字
//
// `ErrorKind` 会被写进日志（String()）与序列化进诊断信息。
// 取值一变，历史日志与新日志就无法比对。更重要的是：它必须是
// **稳定且显式**的 —— 上游的翻译层（toGatewayKind）是显式 switch，
// 但任何人都可能写 `gateway.ErrorKind(someInt)`。
//
// 所以这里的期望值是**字面量的先后顺序**（0..7），
// 与 upstream.ErrKind 的声明顺序一致（见 errorclassifier.go 的注释）。
func TestErrorKindValuesAreStable(t *testing.T) {
	want := []struct {
		kind ErrorKind
		v    int
		name string
	}{
		{ErrKindNone, 0, "none"},
		{ErrKindHardCredit, 1, "hard_credit"},
		{ErrKindSoftRate, 2, "soft_rate"},
		{ErrKindSessionDead, 3, "session_dead"},
		{ErrKindNotFound, 4, "not_found"},
		{ErrKindAuth, 5, "auth"},
		{ErrKindServer, 6, "server"},
		{ErrKindClient, 7, "client"},
	}
	for _, c := range want {
		if int(c.kind) != c.v {
			t.Errorf("%s 的取值 = %d，want %d —— "+
				"取值漂移会让按整数传递/比对的代码静默错位", c.name, int(c.kind), c.v)
		}
		if got := c.kind.String(); got != c.name {
			t.Errorf("ErrorKind(%d).String() = %q，want %q", int(c.kind), got, c.name)
		}
	}
}

// TestDefaultErrorKindIgnoresBody 兜底分类**只看状态码**。
//
// # 这是本扩展点最重要的判据之一
//
// 兜底是"没有分类器的上游"的路径。如果兜底里塞进任何一家的正文关键词，
// 那个上游的错误体就会被**另一家的词汇表**解释 ——
// 正是 P2 缺口（拿 workbuddy 的判据判 codearts）。
//
// 所以这里用 workbuddy 与 codearts 各自的关键词去喂它，
// 断言**状态码说了算，正文完全不影响结果**。
func TestDefaultErrorKindIgnoresBody(t *testing.T) {
	// 同一状态码下，正文怎么变都不该改变分类。
	bodies := []string{
		"",
		`{"code":12153,"msg":"Offline user session not found"}`, // workbuddy 的 session dead
		`{"error_code":"InferHub.4291.200","error_msg":"insufficient quota"}`, // codearts 额度
		`{"msg":"余额不足"}`,
		`{"msg":"insufficient credit"}`,
		"12153",
	}
	cases := []struct {
		status int
		want   ErrorKind
	}{
		{402, ErrKindHardCredit},
		{429, ErrKindSoftRate},
		{404, ErrKindNotFound},
		{500, ErrKindServer},
		{503, ErrKindServer},
		{400, ErrKindClient},
		{403, ErrKindClient},
		{401, ErrKindClient}, // ⚠ 兜底**不**把 401 判成 session dead —— 那是 workbuddy 的正文判据
		{200, ErrKindNone},
	}
	for _, c := range cases {
		for _, b := range bodies {
			if got := DefaultErrorKind(c.status, b); got != c.want {
				t.Errorf("DefaultErrorKind(%d, %q) = %v，want %v —— "+
					"兜底**不得**读正文（读正文 = 用某一家的词汇表解释所有上游）",
					c.status, b, got, c.want)
			}
		}
	}
}

// TestDefaultErrorKindNeverDisables 兜底**永不**产生 SessionDead。
//
// # 为什么这是硬判据
//
// ErrKindSessionDead 在 core 侧映射到 `Pool.Disable`（**永久禁用**，
// 需人工重登）—— 是本系统里后果最重的动作。
//
// 兜底是一条"我不认识这个上游"的路径。一条不认识对方的路径
// **没有资格**做出永久禁用的判决：它只能按 RFC 定义的状态码语义分类。
//
// 这条测试是"永久禁用只能来自上游自己的明确声明"这一原则的载体。
func TestDefaultErrorKindNeverDisables(t *testing.T) {
	for status := 0; status < 600; status++ {
		for _, b := range []string{"", "12153", "Offline user session not found"} {
			if got := DefaultErrorKind(status, b); got == ErrKindSessionDead {
				t.Fatalf("DefaultErrorKind(%d, %q) = session_dead —— "+
					"通用兜底**绝不能**产出永久禁用分类（那是上游自己的声明）", status, b)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 扩展点的发现机制（ExtOf）
// ---------------------------------------------------------------------------

// classifierOnly 是"实现了 Provider、同时也实现了 ErrorClassifier"的对象。
//
// # 为什么必须同时实现 Provider
//
// 因为 `gateway.ExtOf[T](p Provider)` 的入参是 Provider —— 这是刻意的：
// 扩展点挂在**上游实现**上，而不是挂在任意对象上。所以本类型内嵌
// goodProvider（本包已有的合规实现桩）来满足 Provider，再加 Classify。
//
// 它检验的是"扩展点用类型断言发现"这条机制本身。
type classifierOnly struct {
	goodProvider
	kind ErrorKind
	// seenStatus/seenBody 记录最后一次调用参数，验证转发没有丢参数。
	seenStatus int
	seenBody   string
	calls      int
}

func (c *classifierOnly) Classify(status int, body string) ErrorKind {
	c.calls++
	c.seenStatus = status
	c.seenBody = body
	return c.kind
}

// TestExtOfFindsErrorClassifier 扩展点能被类型断言发现（机制本身）。
func TestExtOfFindsErrorClassifier(t *testing.T) {
	c := &classifierOnly{goodProvider: goodProvider{id: "ca"}, kind: ErrKindHardCredit}
	got, ok := ExtOf[ErrorClassifier](c)
	if !ok {
		t.Fatal("ExtOf[ErrorClassifier] 没认出实现了该接口的对象 —— " +
			"扩展点机制失效（P2 的分派就依赖它）")
	}
	if k := got.Classify(402, "insufficient quota"); k != ErrKindHardCredit {
		t.Errorf("转发后分类 = %v，want hard_credit", k)
	}
	if c.seenStatus != 402 || c.seenBody != "insufficient quota" {
		t.Errorf("参数在转发中丢失: status=%d body=%q", c.seenStatus, c.seenBody)
	}
}

// TestExtOfRejectsNonClassifier 没实现该接口的对象必须被判为"没有"。
//
// 这是"不实现不算错"这条语义的载体：调用方拿到 ok=false 后
// 回落到 upstream.Classify（core 的明确取舍），而不是 panic 或猜。
func TestExtOfRejectsNonClassifier(t *testing.T) {
	if _, ok := ExtOf[ErrorClassifier](&goodProvider{id: "plain"}); ok {
		t.Error("一个没实现 ErrorClassifier 的 Provider 被认成了分类器 —— " +
			"那会让 core 调用一个不存在的方法（或拿到零值分类）")
	}
	// nil Provider 不得 panic（ExtOf 有 nil 保护）。
	if _, ok := ExtOf[ErrorClassifier](nil); ok {
		t.Error("nil Provider 不该被认成分类器")
	}
}

// TestErrorClassifierInterfaceIsMinimal 接口必须只有 Classify 一个方法。
//
// # 为什么钉住方法数
//
// 接口越大，上游被迫实现的东西越多，"加新上游核心零改动"这条判据就越难守。
// 本扩展点只解决一件事：把 (status, body) 判成一个中立类别。
// 任何附加方法（例如"告诉我你的错误码表"）都会让它变成浅接口。
//
// 用编译期断言表达：一个只带 Classify 的类型必须满足它。
// 若有人给接口加了方法，下面这行会立刻编译失败。
func TestErrorClassifierInterfaceIsMinimal(t *testing.T) {
	var _ ErrorClassifier = (*classifierOnly)(nil)
}
