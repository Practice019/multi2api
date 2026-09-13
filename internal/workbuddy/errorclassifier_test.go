package workbuddy

// errorclassifier_test.go —— workbuddy 的错误分类扩展点。
//
// # 本文件的判据：**行为逐字不变**
//
// 本次改动把"错误分类"从 core 的一个硬编码调用，改成按上游分派。
// 对 workbuddy 而言，它的判据必须**一字不改** —— 否则就是
// "修好新的、弄坏旧的"（本仓反复出现的那类回归）。
//
// 判据的表达方式：`Provider.Classify(status, body)` 的结果，
// 必须与直接调 `upstream.Classify(status, body)` 再翻译类型**逐项相等**。
//
// 也就是说：本扩展点是**纯搬运**，不引入任何新判据。

import (
	"testing"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// TestWorkbuddyClassifyMatchesUpstreamClassify 是**逐字不变**的主闸门。
//
// # 判据
//
// 对任意 (status, body)，`p.Classify(...)` == 翻译(upstream.Classify(...))。
//
// 换句话说：加了扩展点之后，workbuddy 的分类行为**一个比特都没变**。
// 这正是"改造不回归"的形式化表达 —— 它不是靠抽查几个用例，
// 而是靠**与旧实现的直接对照**。
//
// # 为什么必须存在这一条
//
// 本次改动的风险面正是"默认上游"：单上游部署（既有部署全是这个形态）
// 走的就是 workbuddy 的路径。任何一个判据的细微漂移都会：
//
//	漏判 session dead → 账号不再被禁用 → 用户拿一个死号反复失败
//	误判 session dead → 账号被永久禁用 → 比漏判更糟
//
// # 变异验证（必须红）
//
// 把 `toGatewayKind` 里任意一项改错（例如 ErrSessionDead → ErrKindClient）：
// → 本测试在对应行红 ✓
//
// 若把 `Provider.Classify` 改成"不调 upstream.Classify，自己按状态码判"：
// → 含 12153 的行会红（session dead 判不出来）✓
func TestWorkbuddyClassifyMatchesUpstreamClassify(t *testing.T) {
	p := NewWithConfig(Config{})

	// 这张表同时覆盖两个方向的边界：
	//   - workbuddy 的**正文判据**（12153 / Offline user session / 余额关键词）
	//   - 纯状态码判据（429/404/5xx/4xx）
	//   - 200 + 业务码（"HTTP 200 但 business code 非 0"）
	cases := []struct {
		status int
		body   string
	}{
		// session dead（workbuddy 专有：裸数字 12153 与英文原文）
		{401, `{"code":12153,"msg":"Offline user session not found"}`},
		{401, `Offline user session not found`},
		{200, `{"code":12153}`}, // ⚠ 200 也判 session dead —— 这是既有判据（正文优先于状态码）
		{429, `12153`},          // ⚠ 429 也判 session dead —— 同上
		// 额度
		{402, ``},
		{400, `{"code":1,"msg":"余额不足"}`},
		{403, `insufficient credits`},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`},
		{400, `{"code":1,"msg":"额度用尽"}`},
		{200, `{"msg":"quota exceeded"}`},
		// 状态码
		{429, ``},
		{404, ``},
		{500, `boom`},
		{503, `unavailable`},
		{401, `{"code":9999,"msg":"bad token"}`}, // 不是 session dead → client
		{200, ``},
		{200, `{"ok":true}`},
	}
	for _, c := range cases {
		want := upstreamToGatewayForTest(upstream.Classify(c.status, c.body))
		got := p.Classify(c.status, c.body)
		if got != want {
			t.Errorf("Classify(%d, %q) = %v，但 upstream.Classify 给出 %v ——\n"+
				"★ workbuddy 的分类行为**必须逐字不变**（本次改动只是搬运，不是重写）。\n"+
				"  任何漂移都会影响单上游部署（既有部署的形态）。",
				c.status, c.body, got, want)
		}
	}
}

// upstreamToGatewayForTest 是**测试侧**的独立镜像实现。
//
// # 为什么不直接调 toGatewayKind
//
// 因为那会让本测试变成同义反复：生产代码与测试代码共用同一个翻译函数，
// 函数本身错了两边一起错，测试永远绿。
//
// 这里刻意**手写**一份等价的映射，让"生产翻译"与"测试期望"相互独立 ——
// 两者不一致时测试红，说明其中一份被改错了（哪一份需要人判断）。
//
// 它与生产版 toGatewayKind 的差异只有一处：**没有 default 分支的
// 通用性**，而是直接 panic —— 测试里出现未知 ErrKind 是硬故障，
// 应当炸掉而不是静默返回 none。
func upstreamToGatewayForTest(k upstream.ErrKind) gateway.ErrorKind {
	switch k {
	case upstream.ErrNone:
		return gateway.ErrKindNone
	case upstream.ErrHardCredit:
		return gateway.ErrKindHardCredit
	case upstream.ErrSoftRate:
		return gateway.ErrKindSoftRate
	case upstream.ErrSessionDead:
		return gateway.ErrKindSessionDead
	case upstream.ErrNotFound:
		return gateway.ErrKindNotFound
	case upstream.ErrServer:
		return gateway.ErrKindServer
	case upstream.ErrClient:
		return gateway.ErrKindClient
	default:
		panic("测试镜像没有覆盖 upstream.ErrKind 的新取值 —— 生产翻译也要同步更新")
	}
}

// knownUpstreamKinds 是 upstream.ErrKind 的**全部合法取值**。
//
// # 为什么硬编码这一份（而不遍历 0..N）
//
// "未知取值"与"合法取值"在 `upstream.ErrKind` 上不可区分 ——
// 它没有 `IsValid()`，"超出范围"也只是另一个 int。
// 遍历 0..16 会把 7..15 全部误报成"未覆盖"（我第一版就是这么写的，红了）。
//
// 所以把它**显式列出**，并由 TestKnownUpstreamKindsIsComplete 反向校验：
// 一旦 upstream 加了新常量，`String()` 会给出一个不在本表里的名字，
// 那条测试会红，逼人同步更新这里与两处翻译。
var knownUpstreamKinds = []upstream.ErrKind{
	upstream.ErrNone,
	upstream.ErrHardCredit,
	upstream.ErrSoftRate,
	upstream.ErrSessionDead,
	upstream.ErrNotFound,
	upstream.ErrServer,
	upstream.ErrClient,
}

// TestToGatewayKindCoversEveryUpstreamKind 每个合法取值都有显式翻译分支。
//
// # 为什么必须存在
//
// `toGatewayKind` 有 default 分支（返回 ErrKindNone，保守不惩罚）。
// 那意味着**新增常量会静默落到 default** —— 只抽查几个已有值的测试
// 永远发现不了。这里逐个合法取值断言生产翻译 == 测试镜像（独立手写），
// 两者不一致即红。
//
// # 变异验证（必须红）
//
// 删掉 `toGatewayKind` 里任意一个 case（例如 ErrSessionDead）：
// → 生产落 default(ErrKindNone)，测试镜像给出 ErrKindSessionDead → 红 ✓
func TestToGatewayKindCoversEveryUpstreamKind(t *testing.T) {
	for _, k := range knownUpstreamKinds {
		want := upstreamToGatewayForTest(k)
		if got := toGatewayKind(k); got != want {
			t.Errorf("toGatewayKind(%v) = %v，测试镜像给出 %v —— "+
				"说明生产翻译少了一个显式分支（它会落到 default）", k, got, want)
		}
	}
}

// TestKnownUpstreamKindsIsComplete 反向校验上面的表**没有漏项**。
//
// # 判据
//
// 枚举 0..15，凡 `String()` 给出"专属名字"（非 "none" 兜底…见下）的都是合法取值。
//
// ⚠ `String()` 对 `ErrNone` 返回 "none"，对**未知值**也返回 "none"
// （见 upstream/client.go 的 default 分支）—— 所以不能只看名字。
// 改用另一个可靠信号：**该取值是否出现在 knownUpstreamKinds 里**，
// 再断言"名字互不相同"。真正要夹住的是：
//
//	upstream 新增常量 → 名字集合变大 → 本测试红
//
// 用一个"名字集合大小 == 已知表大小"的等式就能表达它。
func TestKnownUpstreamKindsIsComplete(t *testing.T) {
	seen := map[string]upstream.ErrKind{}
	for _, k := range knownUpstreamKinds {
		seen[k.String()] = k
	}
	// 已知表内部不能有重名（否则翻译必然有二义性）。
	if len(seen) != len(knownUpstreamKinds) {
		t.Fatalf("knownUpstreamKinds 里有重名: %d 个取值但只有 %d 个不同的名字",
			len(knownUpstreamKinds), len(seen))
	}

	// 扫描一个有余量的整数范围，找出**表外**且名字非 "none" 的取值 ——
	// 那就是 upstream 新增了常量而本表没跟上。
	for i := 0; i < 32; i++ {
		k := upstream.ErrKind(i)
		name := k.String()
		if name == "none" {
			// ErrNone 与所有未知值都返回 "none"，无法区分；
			// ErrNone 已在表里，其余忽略。
			continue
		}
		if _, ok := seen[name]; !ok {
			t.Errorf("upstream.ErrKind(%d) 的名字是 %q，不在 knownUpstreamKinds 里 ——\n"+
				"  upstream 新增了常量，本测试表与两处翻译都要同步更新：\n"+
				"    internal/workbuddy/errorclassifier.go 的 toGatewayKind\n"+
				"    internal/server/handler.go 的 upstreamToGateway / upstreamKindOf", i, name)
		}
	}
}

// TestClassifyNeverPanics 分类器对任意输入都不得 panic。
//
// # 为什么（契约要求）
//
// gateway.ErrorClassifier 明确要求不得 panic（与 Provider 的契约一致）。
// 它在出站循环里被调用 —— 一次 panic 会带崩整个请求（甚至进程）。
//
// 上游的响应体是**外部输入**：可能为空、可能是任意二进制、
// 可能超长、可能是畸形 JSON。分类器必须对这些全部免疫。
//
// # 变异验证（必须红）
//
// 在 Classify 里加一行 `if body == "" { panic("x") }`：
// → 本测试红 ✓
func TestClassifyNeverPanics(t *testing.T) {
	p := NewWithConfig(Config{})

	inputs := []string{
		"",
		"\x00\x01\x02",
		"12153",
		"{\"unclosed",
		"\xff\xfe\xfd",
		repeat("a", 100000),
		`{"code":12153,"msg":"` + repeat("-", 10000) + `"}`,
	}
	statuses := []int{0, -1, 200, 301, 400, 401, 402, 403, 404, 429, 500, 599, 99999}

	for _, st := range statuses {
		for _, in := range inputs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Classify(%d, <len=%d>) panic: %v —— "+
							"分类器对**任意**上游响应体都不得 panic（它在请求路径上）",
							st, len(in), r)
					}
				}()
				_ = p.Classify(st, in)
			}()
		}
	}
}

// repeat 是 strings.Repeat 的本地包装（避免为一条测试引入 import）。
func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
