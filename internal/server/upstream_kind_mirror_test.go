// upstream_kind_mirror_test.go core 侧两张翻译表的完备性判据。
//
// # 为什么必须补这个文件（它不是"锦上添花的测试"）
//
// handler.go 里 `upstreamErrKindMirror` 的注释写着：
//
//	风险（枚举漂移）由两侧的测试共同钉住：
//	  internal/server 的 TestUpstreamKindMirrorCoversEveryKind
//	  internal/workbuddy 的 TestToGatewayKindCoversEveryUpstreamKind
//
// 而 **TestUpstreamKindMirrorCoversEveryKind 从来不存在**。
// 也就是说，core 侧那张表（`upstreamToGateway`）一直是**无保护**的。
//
// 这不是纯理论问题：本次新增 `upstream.ErrContentBlocked` 时，
// 我只改了 `upstreamKindOf`（翻回 ErrKind，只用于日志）而漏了
// `upstreamToGateway`（翻成中立类型，**参与策略判断**）——
//
//	因为 workbuddy 侧那条 guard 只保护 workbuddy 的表；
//	core 侧没有任何东西会红。
//	结果：单上游部署下内容拦截被翻成 ErrKindNone → "只换号不罚"
//	→ 一次内容拦截白烧 MaxRotate 次往返 → 503 "所有账号不可用"。
//
// 最终是 degrade_test.go 的端到端用例把它抓出来的（那是**行为**测试，
// 不是**表**测试）。本文件补上表这一层的直接保护：
// 新增枚举值时立刻红，而不是等到某条端到端路径恰好覆盖到。
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

// TestUpstreamKindMirrorCoversEveryKind core 的翻译表覆盖每一个 upstream 取值。
//
// # 判据
//
// 除 ErrNone 外，每个取值都必须翻到一个**非 ErrKindNone** 的中立类型。
//
// 为什么"非 None"是关键判据：ErrKindNone 在 core 侧的含义是
// "只换号不惩罚"。一个新错误类别如果静默落到 None，
// 它在日志里看起来"处理过了"，实际**完全没被区别对待** ——
// 那正是本次内容拦截踩的坑。
//
// ⚠ 这条断言对"两张表的方向"都成立：
//
//	upstreamToGateway  参与策略判断（漏了会误判）
//	upstreamKindOf     只影响日志文本（漏了只是不好读）
//
// 但两者都用同一份枚举做输入，所以这里对两张表分别断言。
func TestUpstreamKindMirrorCoversEveryKind(t *testing.T) {
	for _, k := range allUpstreamKinds() {
		got := upstreamToGateway(k)

		if k == upstream.ErrNone {
			if got != gateway.ErrKindNone {
				t.Errorf("upstreamToGateway(ErrNone) = %v，期望 none", got)
			}
			continue
		}
		if got == gateway.ErrKindNone {
			t.Errorf("upstreamToGateway(%v) = none —— "+
				"core 会把 %q 这类错误当成\"只换号不惩罚\"，"+
				"等于**完全没有区别对待**它。\n"+
				"  请给 upstreamToGateway 补一个显式 case。", k, k.String())
		}
	}
}

// TestUpstreamKindOfCoversEveryKind 反向表（中立 → upstream）同样必须覆盖。
//
// 它的用途只是拼日志文本，所以"漏了"的后果比上面轻 ——
// 但漏了会让日志里出现 "upstream none (http 400)"，
// 而真实原因是内容拦截。可诊断性也是判据的一部分。
func TestUpstreamKindOfCoversEveryKind(t *testing.T) {
	// 中立类型里有的、upstream 没有的取值（Auth），单独排除：
	// 它翻回 ErrNone 是**刻意**的（见 upstreamKindOf 的注释）。
	skip := map[gateway.ErrorKind]bool{gateway.ErrKindAuth: true}

	for _, k := range allUpstreamKinds() {
		if k == upstream.ErrNone {
			continue
		}
		mid := upstreamToGateway(k)
		if mid == gateway.ErrKindNone || skip[mid] {
			continue
		}
		// 往返：upstream → mid → upstream，应回到原值。
		if back := upstreamKindOf(mid); back != k {
			t.Errorf("往返不一致：%v → %v → %v（期望回到 %v）—— "+
				"两张表的方向不同步", k, mid, back, k)
		}
	}
}

// TestUpstreamToGatewayAgainstHandWrittenMirror 与**独立手写**的镜像比对。
//
// # 为什么不直接断言 `upstreamToGateway(k) != None` 就够了
//
// 那只挡住"漏项"，挡不住"翻错项"（例如把 ContentBlocked 翻成 HardCredit，
// 那会让内容拦截去冷却账号 —— 比漏项更糟）。
//
// 这里手写一份**测试侧**的期望，与生产代码相互独立：
// 两者不一致时红，由人来判断哪一份错了。
//
// 反向判别力：把生产表里 ErrContentBlocked 的 case 改成返回
// gateway.ErrKindHardCredit → 本用例红 ✓
func TestUpstreamToGatewayAgainstHandWrittenMirror(t *testing.T) {
	want := map[upstream.ErrKind]gateway.ErrorKind{
		upstream.ErrNone:           gateway.ErrKindNone,
		upstream.ErrHardCredit:     gateway.ErrKindHardCredit,
		upstream.ErrSoftRate:       gateway.ErrKindSoftRate,
		upstream.ErrSessionDead:    gateway.ErrKindSessionDead,
		upstream.ErrNotFound:       gateway.ErrKindNotFound,
		upstream.ErrServer:         gateway.ErrKindServer,
		upstream.ErrClient:         gateway.ErrKindClient,
		upstream.ErrContentBlocked: gateway.ErrKindContentBlocked,
	}
	for k, w := range want {
		if got := upstreamToGateway(k); got != w {
			t.Errorf("upstreamToGateway(%v) = %v，手写镜像期望 %v", k, got, w)
		}
	}
	// 手写表本身也要覆盖全部枚举（否则它自己就是个漏项的弱判据）。
	for _, k := range allUpstreamKinds() {
		if _, ok := want[k]; !ok {
			t.Errorf("手写镜像缺 %v —— 本用例的覆盖度不足", k)
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
	if upstreamToGateway(upstream.ErrContentBlocked) == upstreamToGateway(upstream.ErrClient) {
		t.Fatal("content_blocked 被翻成了与 client 相同的类别 —— " +
			"内容问题会走换号路径，白烧轮换预算并把内容问题误报成账号故障")
	}
	if gateway.ErrKindContentBlocked == gateway.ErrKindClient {
		t.Fatal("gateway 侧两个常量取值相同")
	}
}
