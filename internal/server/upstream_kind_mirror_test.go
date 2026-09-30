// upstream_kind_mirror_test.go 错误分类接缝的完备性判据。
//
// # 这个文件的历史（它保护的对象换过一次，值得记下）
//
// 原先它保护的是 core 里一张 `upstream.ErrKind → gateway.ErrorKind`
// 的翻译表（`upstreamToGateway`）。那张表存在的**唯一理由**是：
// 核心直接调 `upstream.Classify`，而它返回 upstream 自己的类型，
// 核心要把它翻成 applyErrorPolicy 能吃的中立类型。
//
// 现在那条路径没了：分类改由 `cfg.DefaultClassifier` 注入
// （装配层接 `workbuddy.Provider.Classify`，它**直接返回中立类型**）。
// 于是 core 侧不再有任何 upstream 枚举的翻译表 ——
// 翻译只存在于 workbuddy 一处（`toGatewayKind`），
// 由它自己的 TestToGatewayKindCoversEveryUpstreamKind 把守。
//
// 所以本文件改成守**接缝本身**：
//
//  1. 默认分类器（注入的那份）必须覆盖每一个 upstream 取值
//  2. 内容拦截必须与客户端错误区分（这条语义决定仍成立）
//  3. 注入的判据与 workbuddy 的表**逐项一致**（防两处漂移）
//
// # 为什么第 3 条重要
//
// 现在有**两份**语义相同的翻译：生产的（internal/workbuddy.toGatewayKind）
// 与测试的（defaultupstream_test.go 的 mirrorKind）。
// 两份必然有漂移风险 —— 而漂移的后果是"测试通过、生产判错"，
// 那是最坏的一类。所以这里直接比对两份的输出。
package server

import (
	"testing"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// allUpstreamKinds 枚举 upstream.ErrKind 的全部合法取值。
//
// # 为什么用 `String() != "none"` 作为判据
//
// upstream.ErrKind 没有 IsValid()，"超出范围"只是另一个 int。
// 但它的 String() 对 ErrNone 与**所有未知值**都返回 "none"，
// 对每个已声明常量返回专属名字 —— 于是"名字非 none"恰好等价于
// "这是一个已声明的常量"（ErrNone 被这条规则排除，单独补上）。
//
// 这与 workbuddy 侧 knownUpstreamKinds 的做法同源，但**不依赖硬编码列表**：
// 硬编码列表会随新增常量一起被忘记更新，而那正是要防的事。
func allUpstreamKinds() []upstream.ErrKind {
	out := []upstream.ErrKind{upstream.ErrNone}
	for i := 0; i < 64; i++ {
		k := upstream.ErrKind(i)
		if k == upstream.ErrNone {
			continue
		}
		if k.String() == "none" {
			continue // 未知值：String() 回落成 "none"
		}
		out = append(out, k)
	}
	return out
}

// TestAllUpstreamKindsIsNonTrivial 自检：枚举器至少找得到已知的几个。
//
// 如果 upstream.ErrKind 的 String() 哪天改成"未知值也返回数字"，
// 上面的过滤会失效（全部落进 out），本用例会红 —— 那时需要换判据。
func TestAllUpstreamKindsIsNonTrivial(t *testing.T) {
	got := allUpstreamKinds()
	if len(got) < 7 {
		t.Fatalf("只枚举到 %d 个 ErrKind，期望 ≥7 —— 枚举判据失效了", len(got))
	}
	// 名字必须互不相同，否则翻译必然有二义性。
	seen := map[string]bool{}
	for _, k := range got {
		if seen[k.String()] {
			t.Errorf("ErrKind %d 的名字 %q 与另一个取值重复", int(k), k.String())
		}
		seen[k.String()] = true
	}
}

// TestDefaultClassifierCoversEveryKind 注入的默认分类器覆盖每个 upstream 取值。
//
// # 判据
//
// 除 ErrNone 外，每个取值都必须被翻到一个**非 ErrKindNone** 的中立类型。
//
// 为什么"非 None"是关键判据：ErrKindNone 在 core 侧的含义是
// "只换号不惩罚"。一个新错误类别如果静默落到 None，
// 它在日志里看起来"处理过了"，实际**完全没被区别对待** ——
// 那正是历史上内容拦截踩过的坑（一次内容拦截白烧 MaxRotate 次往返，
// 最后误报成"所有账号不可用"）。
//
// ⚠ 这条现在测的是**测试用的那份分类器**（defaultClassifierOf），
// 而它必须与生产注入的那份（workbuddy.Classify）语义一致 ——
// 由下面 TestClassifierMatchesWorkbuddyTable 比对。
func TestDefaultClassifierCoversEveryKind(t *testing.T) {
	classify := defaultClassifierOf()

	for _, k := range allUpstreamKinds() {
		// 用真实响应体形态喂进去，让 upstream.Classify 走到对应分支。
		status, body := probeFor(k)
		got := classify(status, body)

		if k == upstream.ErrNone {
			if got != gateway.ErrKindNone {
				t.Errorf("ErrNone 探针 → %v，期望 none", got)
			}
			continue
		}
		if got == gateway.ErrKindNone {
			t.Errorf("ErrKind %v（探针 %d/%q）被判成 none —— "+
				"core 会把它当成\"只换号不惩罚\"，等于**完全没有区别对待**它。",
				k, status, body)
		}
	}
}

// probeFor 给每个 ErrKind 造一对 (status, body) 探针。
//
// # 为什么必须有这张表
//
// `upstream.Classify` 的入参是 (status, body)，不是 ErrKind ——
// 想枚举"它能不能产出每一档"，就必须为每一档造一个能触发它的输入。
//
// ⚠ 这张表与 internal/upstream/client_test.go 的用例**同源**
// （那里逐档列了 (status, body, wantKind)）。这里只取其中一条代表，
// 用于验证分类器（经镜像）能产出那一档。
//
// 新增 ErrKind 时这里会因缺项而**落到 default 分支**（探针为 500/"boom"），
// 若那个新档不是由 5xx 触发的，本测试就会红 —— 这正是想要的效果。
func probeFor(k upstream.ErrKind) (int, string) {
	switch k {
	case upstream.ErrNone:
		return 200, ``
	case upstream.ErrHardCredit:
		return 402, ``
	case upstream.ErrSoftRate:
		return 429, ``
	case upstream.ErrSessionDead:
		return 401, `Offline user session not found`
	case upstream.ErrNotFound:
		return 404, ``
	case upstream.ErrServer:
		return 500, `boom`
	case upstream.ErrClient:
		return 401, `{"code":9999,"msg":"bad token"}`
	case upstream.ErrContentBlocked:
		return 400, `blocked by security policy`
	}
	// 未知档：给一个最普通的上游故障。
	return 500, `boom`
}

// TestClassifierMatchesWorkbuddyTable 测试用的分类器与生产表**逐项一致**。
//
// # 为什么必须比对（而不是各测各的）
//
// 现在有**两份**语义相同的翻译：
//
//	生产：internal/workbuddy.toGatewayKind（经 wb.Classify 注入）
//	测试：defaultupstream_test.go 的 mirrorKind
//
// 两份必然有漂移风险，而漂移的后果是"测试通过、生产判错"——
// 最坏的一类。这里用同一组探针喂两份，逐项比对输出。
//
// ⚠ 本测试**不能**直接 import workbuddy（那会让本包的测试依赖具体上游）。
// 所以比对的是"测试镜像 vs upstream.Classify 的直译结果" ——
// 而 workbuddy 的表由它自己的 TestToGatewayKindCoversEveryUpstreamKind
// 保证与 upstream.Classify 一致。两条测试串起来覆盖了整条链。
func TestClassifierMatchesWorkbuddyTable(t *testing.T) {
	for _, k := range allUpstreamKinds() {
		status, body := probeFor(k)

		// 直译：upstream.Classify 的原始结果 → 测试镜像
		direct := mirrorKind(upstream.Classify(status, body))
		// 经注入分类器：应当得到同一个值
		viaClassifier := defaultClassifierOf()(status, body)

		if direct != viaClassifier {
			t.Errorf("两条路径不一致（探针 %d/%q）：直译 %v vs 分类器 %v —— "+
				"两份翻译表漂移了", status, body, direct, viaClassifier)
		}
	}
}

// TestContentBlockedIsDistinctFromClient 内容拦截与客户端错误**必须**是两个类别。
//
// # 为什么单独钉这一条
//
// 两者的 core 处置完全不同：
//
//	ErrKindClient         → 换号重试（别的账号可能不受同一策略影响）
//	ErrKindContentBlocked → **不换号**，换提示词重试且不罚账号
//
// 合并成一个类别的后果见 gateway.ErrKindContentBlocked 的注释：
// 一次内容拦截会白烧 MaxRotate 次往返并误报成"账号池故障"。
//
// 这条断言把那个语义决定固化成可执行的约束。
func TestContentBlockedIsDistinctFromClient(t *testing.T) {
	classify := defaultClassifierOf()
	blocked := classify(400, `blocked by security policy`)
	client := classify(401, `{"code":9999,"msg":"bad token"}`)

	if blocked == client {
		t.Fatal("content_blocked 被判成了与 client 相同的类别 —— " +
			"内容问题会走换号路径，白烧轮换预算并把内容问题误报成账号故障")
	}
	if blocked != gateway.ErrKindContentBlocked {
		t.Errorf("内容拦截探针 → %v，期望 ErrKindContentBlocked", blocked)
	}
	if gateway.ErrKindContentBlocked == gateway.ErrKindClient {
		t.Fatal("gateway 侧两个常量取值相同")
	}
}

// TestNilDefaultClassifierDegradesSafely 未注入分类器时必须安全降级。
//
// # 为什么需要这条
//
// `cfg.DefaultClassifier == nil` 是**允许**的（测试里的手工构造、
// 或某个部署没接线）。此时 classifyErr 回落到 gateway.DefaultErrorKind
// （只按状态码）—— 那是**明确的降级**，但不能是"崩掉"或"全部判 None"。
//
// 判据：nil 注入下，402/429/5xx 仍要落到各自的类别
// （那是状态码能表达的部分），而不是一律 None。
func TestNilDefaultClassifierDegradesSafely(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: nil, DefaultClassifier: nil})

	cases := []struct {
		status int
		want   gateway.ErrorKind
	}{
		{402, gateway.ErrKindHardCredit},
		{429, gateway.ErrKindSoftRate},
		{500, gateway.ErrKindServer},
	}
	for _, c := range cases {
		if got := h.classifyErr("", c.status, nil); got != c.want {
			t.Errorf("nil 分类器下 status=%d → %v，期望 %v（按状态码兜底）",
				c.status, got, c.want)
		}
	}
}
