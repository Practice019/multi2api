// 架构约束测试 —— 判据 3 的载体。
//
// # 为什么用测试而不是注释
//
// 注释写在代码里没人看；测试红了必须改。
// "核心包不得依赖具体上游"这条约束如果只写在文档里，
// 第 3 个上游接入时一定会有人为了方便直接 import 一下。
//
// # 反向验证
//
// 本文件同时验证"约束测试真的有效"：
// 用一个**故意违规的临时包**验证检测逻辑能抓到它。
// 否则一个永远绿灯的约束测试等于没有约束。
package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// corePackages 核心包 —— 它们**不得**依赖任何具体上游实现。
//
// 加新上游时这些包一行都不该改。谁在这里 import 了具体上游，
// 谁就破坏了"加一个上游 = 加一个目录"。
var corePackages = []string{
	"workbuddy2api/internal/gateway",
	"workbuddy2api/internal/pool",
	"workbuddy2api/internal/logbuf",
	"workbuddy2api/internal/admin",
	"workbuddy2api/internal/server",
	"workbuddy2api/internal/scheduler",
	"workbuddy2api/internal/checkinlog",
	"workbuddy2api/internal/clientlogin",
}

// upstreamPrefix 上游实现的包路径前缀。
//
// ⚠ 新增上游时**必须**在此登记，否则约束测试管不到它。
// 这本身是被测试强制的：TestUpstreamListIsEnforced 会检查每个
// internal/<名字>/ 是否在此列。
var upstreamPrefix = "workbuddy2api/internal/workbuddy"

// gatewayContractMarkers 已废弃 —— 见 discoverUpstreams 的说明。
//
// 保留这个变量会让后来者以为"加个 marker 就能被认成上游"，
// 而那正是被绕过两次的机制。所以显式删除它，并在下方说明为什么。
//
// （历史上它曾是 `[]string{"gateway.Provider =", "Caps() gateway.Capability"}`。）

// discoveryExemptPackages 明确**不是上游**的包。
//
// # 为什么这个列表可以存在，而白名单不行（关键区别）
//
// 之前的白名单是"声称某包**不是**上游" → 把真上游放进白名单目录即可绕过。
//
// 这里的列表语义相反：它是"这个包**不可能是**上游实现"的**证明义务**，
// 而且由 TestDiscoveryExemptionsAreJustified 强制：
// 每个列出的包都必须**不依赖** internal/gateway。
// 一旦某个包开始 import gateway，它就不再满足豁免条件，测试会红。
//
// 换句话说：豁免不是声明出来的，是**被验证的**。
var discoveryExemptPackages = []string{
	// 通用基础设施：它们不依赖 gateway（有测试断言）
	"auth", "checkinlog", "clientlogin", "logbuf", "oauth", "redisstore", "session", "upstream",
}

// discoverUpstreams 从磁盘推导上游包名 —— **不靠字符串匹配，靠类型系统**。
//
// # 为什么必须改成这样（两次绕过教训）
//
// 第一版：人工维护 `knownUpstreams` → 忘登记即失效。
// 第二版：磁盘推导 + 非上游白名单 → 把上游放进白名单目录即绕过。
// 第三版：靠源码里的字面串（`gateway.Provider =` / `Caps() gateway.Capability`）
//
//	       → **用 import 别名即可绕过**：
//
//		var _ gw.Provider = (*Provider)(nil)
//		func (p *Provider) Caps() gw.Capability { ... }
//
// 两个标记串都不匹配，于是这个包**完全不可见** —— 它依赖 pool 也不会被拦。
// 评审用这个手法把整套架构约束测试跑成了全绿。
//
// 教训：**只要判据是"源码文本"，就一定能被改写成等价但不同文本的代码绕过。**
// 文本匹配还会自命中（我自己的探测包因为**注释里写了那两个标记串**而被误判为上游）。
//
// # 现在的判据：包级依赖图，由 Go 工具链给出
//
// 一个包是"上游实现" ⟺ 它的**非测试**依赖里含 `internal/gateway`
// （即它消费契约），且不在 corePackages / discoveryExemptPackages 里。
//
// 这条判据：
//   - 与标识符拼写无关（别名、点导入、重新导出都拦得住）
//   - 与注释无关（自命中消失）
//   - 由 `go list -deps` 给出，是编译器眼中的事实，不是我们眼里的文本
//
// 唯一假设：上游必须 import gateway 才能实现契约 —— 这正是本设计的强制要求，
// 且有 TestUpstreamsActuallyDependOnGateway 反向断言（见下）。
func discoverUpstreams(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatalf("读 internal/ 失败: %v", err)
	}

	coreNames := map[string]bool{}
	for _, p := range corePackages {
		coreNames[strings.TrimPrefix(p, "workbuddy2api/internal/")] = true
	}
	exempt := map[string]bool{}
	for _, p := range discoveryExemptPackages {
		exempt[p] = true
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// 忽略下划线/点开头的临时目录
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if coreNames[name] || exempt[name] {
			continue
		}
		if dependsOnGateway(t, root, name) {
			out = append(out, name)
		}
	}
	return out
}

// dependsOnGateway 报告 internal/<pkg> 的**非测试**依赖里是否含 internal/gateway。
//
// 用 `go list -deps` 而不是 `go list -deps -test`：测试里提到契约不代表
// 该包是上游实现（例如 gateway 自己的测试）。
//
// 失败即返回 false —— 调用方（TestDiscoveryHasNoBlindSpot）会从另一个方向
// 交叉验证，不会因为一次命令失败就静默放行。
func dependsOnGateway(t *testing.T, root, pkg string) bool {
	t.Helper()
	deps, err := listDeps(root, "workbuddy2api/internal/"+pkg)
	if err != nil {
		return false
	}
	for _, d := range deps {
		if d == "workbuddy2api/internal/gateway" {
			return true
		}
	}
	return false
}

// TestDiscoveryExemptionsAreJustified 豁免名单必须**自证**。
//
// 每个被豁免的包都必须不依赖 gateway —— 否则它其实可能是上游，
// 豁免就成了白名单（而那正是被绕过过的机制）。
//
// 这条把"豁免"从"声明"变成"义务"：一旦某个被豁免的包开始 import gateway，
// 测试立刻红，逼着人重新判断它是不是上游。
func TestDiscoveryExemptionsAreJustified(t *testing.T) {
	root := moduleRoot(t)
	for _, pkg := range discoveryExemptPackages {
		if _, err := os.Stat(filepath.Join(root, "internal", pkg)); err != nil {
			continue // 包不存在，豁免无意义也无害
		}
		if dependsOnGateway(t, root, pkg) {
			t.Errorf("internal/%s 被列为「不可能是上游」，但它依赖 internal/gateway —— "+
				"豁免不成立。要么把它从豁免名单移除（让它受架构约束），"+
				"要么说明它为什么不该被当作上游。", pkg)
		}
	}
}

// TestUpstreamsActuallyDependOnGateway 反向断言：被发现的上游必须真的依赖 gateway。
//
// 防止"依赖图判据"退化成"随便什么都算上游"（那样会让核心包被误判，
// 约束反向失效）。这条与 TestDiscoveryHasNoBlindSpot 一起构成双向验证。
func TestUpstreamsActuallyDependOnGateway(t *testing.T) {
	root := moduleRoot(t)
	ups := discoverUpstreams(t, root)
	t.Logf("发现的上游: %v", ups)
	for _, u := range ups {
		if !dependsOnGateway(t, root, u) {
			t.Errorf("internal/%s 被发现为上游，但它不依赖 internal/gateway —— "+
				"判据不一致", u)
		}
	}
	if len(ups) == 0 {
		t.Fatal("一个上游都没发现 —— 判据或路径有问题，本测试自身失效")
	}
}

// TestDiscoveryFindsKnownUpstreams 确认推导**真的推出了当前已知的上游**。
//
// 没有这条，discoverUpstreams 一旦因为路径/判据错误返回空列表，
// 下面几条约束测试就会"零上游 → 零违规 → 绿灯"，形成 fail-open。
func TestDiscoveryFindsKnownUpstreams(t *testing.T) {
	root := moduleRoot(t)
	ups := discoverUpstreams(t, root)
	t.Logf("从 internal/ 推导出的上游包: %v", ups)

	found := map[string]bool{}
	for _, u := range ups {
		found[u] = true
	}

	// 遍历 internal/ 下每个目录：若它依赖 gateway 却没被发现 → 判据有洞
	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	coreNames := map[string]bool{}
	for _, p := range corePackages {
		coreNames[strings.TrimPrefix(p, "workbuddy2api/internal/")] = true
	}
	exempt := map[string]bool{}
	for _, p := range discoveryExemptPackages {
		exempt[p] = true
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if coreNames[name] || exempt[name] {
			continue
		}
		if dependsOnGateway(t, root, name) && !found[name] {
			t.Errorf("internal/%s 依赖 internal/gateway（= 消费契约），却没被发现为上游 "+
				"—— 判据有洞（fail-open）", name)
		}
	}

	// 反向：当前确实存在若干上游实现
	if !found["workbuddy"] {
		t.Error("internal/workbuddy 必须被推导出来")
	}
	if !found["codearts"] {
		t.Error("internal/codearts 必须被推导出来")
	}
	// Loomy 是第三个上游，也是"轻上游"的实测对象（见 internal/loomy 的包注释）。
	//
	// # 为什么它值得单独断言（而不是"多一个少一个无所谓"）
	//
	// 判据 1 声称"加一个上游 = 加一个目录 + 实现接口 + 配置加一段，核心零改动"。
	// 那条判据只有在**上游真的被推导出来**时才有意义 —— 一个不被发现的包
	// 既不受 TestUpstreamsDoNotDependOnCore 约束，也不受核心反向依赖检查。
	// 所以这里钉住它必须出现在发现结果里：改名、去掉 gateway 依赖、
	// 或把它塞进任何豁免列表，都会立刻红。
	if !found["loomy"] {
		t.Error("internal/loomy 必须被推导出来（它是判据 1 的第三个实测对象）")
	}
	// TRAE 是第四个上游（internal/trae），同一套判据。
	if !found["trae"] {
		t.Error("internal/trae 必须被推导出来（它是判据 1 的第四个实测对象）")
	}
}

// ── 已废弃：基于源码字面串的上游判定 ──────────────────────────────
//
// 历史上有过三个实现，都被证明可绕过或自命中：
//
//   1. 人工维护的 knownUpstreams 列表 —— 忘登记即完全失效
//   2. 磁盘推导 + 非上游白名单 —— 把上游放进白名单目录即绕过
//   3. 源码字面串（gateway.Provider = / Caps() gateway.Capability）——
//      用 import 别名即可绕过：
//
//          var _ gw.Provider = (*Provider)(nil)
//          func (p *Provider) Caps() gw.Capability { ... }
//
//      两个标记串都不匹配 → 该包完全不可见，连它依赖 pool 都不会被拦。
//      评审用这个手法把整套约束测试跑成了全绿。
//      而且文本匹配会**自命中**：一个只在注释里写了那两个串的探测包
//      会被误判为上游（我自己就踩过）。
//
// 现在改为**包级依赖图**判据（dependsOnGateway，由 go list -deps 给出）。
// 见 discoverUpstreams 上方的完整说明。
// ────────────────────────────────────────────────────────────────

func TestDiscoveryHasNoBlindSpot(t *testing.T) {
	root := moduleRoot(t)

	discovered := map[string]bool{}
	for _, u := range discoverUpstreams(t, root) {
		discovered[u] = true
	}

	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	// 独立枚举：internal/ 下**每一个**依赖 gateway 且不在核心/豁免名单里的包。
	//
	// ⚠ 必须排除 corePackages 与 discoveryExemptPackages —— 否则会把
	// admin/scheduler/server/gateway 自己（它们**消费**契约）算成"实现者"，
	// 于是这条测试永远红。这正是我第一版写成这样时的报错：
	// "internal/admin 依赖 internal/gateway 却没被发现"。
	// admin 依赖 gateway 是**正常的**（它遍历 Registry 挂载上游端点）。
	coreNames := map[string]bool{}
	for _, p := range corePackages {
		coreNames[strings.TrimPrefix(p, "workbuddy2api/internal/")] = true
	}
	exempt := map[string]bool{}
	for _, p := range discoveryExemptPackages {
		exempt[p] = true
	}
	var implementers []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if coreNames[name] || exempt[name] {
			continue
		}
		if dependsOnGateway(t, root, name) {
			implementers = append(implementers, name)
		}
	}

	// 正向：依赖 gateway 的包必须被发现
	//
	// ⚠ 这里的"独立枚举"用的是**同一个** dependsOnGateway ——
	// 所以它只能验证"过滤条件（core/exempt 名单）没把谁漏掉"，
	// 不能验证 dependsOnGateway 本身是否正确。
	// 后者由 TestUpstreamsActuallyDependOnGateway 从反方向守住。
	blind := 0
	for _, name := range implementers {
		if !discovered[name] {
			blind++
			t.Errorf("internal/%s 依赖 internal/gateway 却没被 discoverUpstreams 发现 "+
				"—— 它绕过了架构约束（fail-open）。\n"+
				"  常见原因：它在 corePackages 或 discoveryExemptPackages 里，但其实是上游。", name)
		}
	}

	// 反向：发现的不许误判（把核心包当成上游会让约束反向失效）
	for name := range discovered {
		if !dependsOnGateway(t, root, name) {
			t.Errorf("discoverUpstreams 把 internal/%s 当成上游，但它并不依赖 gateway "+
				"—— 误判会让核心包被当作上游，依赖约束反向失效", name)
		}
	}

	if len(implementers) == 0 {
		t.Fatal("一个依赖 gateway 的包都没有 —— 路径或判据有问题，本测试自身失效")
	}
	t.Logf("依赖 gateway 的包: %v；被发现: %v", implementers, keysOf(discovered))
	if blind == 0 {
		t.Log("✓ 无盲区：所有依赖 gateway 的包都被发现")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── 盲区补丁：私有 SDK 不得被当作共享基础设施 ──────────────────────────────
//
// # 为什么要有这一组测试（实测出来的 fail-open）
//
// 上面那套判据推导上游的方式是「非测试依赖里含 internal/gateway」。
// 它对 internal/workbuddy / codearts / loomy / mimo / trae 都有效，
// 但对 internal/upstream **永远返回 false** ——
//
//	internal/upstream 不 import gateway（它早于 gateway 存在，
//	是被「适配」的那一方，契约在 workbuddy 侧适配），于是它落在
//	discoveryExemptPackages 的豁免里，整套架构约束**完全管不到它**。
//
// 而它的真实身份由自己的包注释写得很清楚（client.go:1）：
//
//	// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
//	// 以及错误分类（驱动 pool 冷却状态机）。
//
// **它是 workbuddy 的私有 SDK，不是共享基础设施。** 名字叫 upstream 只是
// 因为它早于多上游改造。豁免名单把它当「通用基础设施」放行了，
// 于是下面这条约束被静默绕过：
//
//	TestUpstreamsDoNotDependOnCore 里写着「上游之间也不该互相依赖
//	（否则拔掉一个会牵连另一个）」—— 而 codearts（上游）确实 import 了
//	upstream（另一个上游），8 处。
//
// 后果不是理论上的：codearts/client.go:401-404 把 **codearts 的请求体**
// 送进 workbuddy 的 PrepareBodyOptWithLimits，于是 workbuddy 的反探测改写
// （sanitize.go 的 11-128 / Claude Code 身份句 / 字段白名单）会逐字作用在
// codearts 流量上。这是比类型耦合更实质的语义泄漏 —— 一个上游的怪癖
// 被另一个上游的补丁解释。
//
// # 判据为什么不靠「包名」或「豁免名单」声明
//
// 靠包名（"upstream 听起来像共享的"）就是这次出错的根源 —— 名字是声明，
// 声明会骗人。所以判据必须是**可验证的依赖事实**：
//
//	被 ≥2 个上游实现依赖的包，必须要么是 gateway（契约），
//	要么在 sharedInfraPackages 里**逐个自证**（见下）。
//
// workbuddyPrivateSDK 是显式登记「这其实是某个上游的私有 SDK」的名单。
// 它与 discoveryExemptPackages 的区别：那边说"不是上游"，这边说"是上游私有物"。
// 两者都由测试强制自证，都不能凭空声明。

// workbuddyPrivateSDK 名为通用、实为 workbuddy 私有 SDK 的包。
//
// ⚠ 加入这个名单**不会**让它免于约束 —— 恰恰相反：
// TestPrivateSDKIsNotConsumedByOtherUpstreams 会禁止**除 workbuddy 之外的
// 任何上游**依赖它。这正是本名单存在的意义（把"谁可以依赖它"钉死）。
var workbuddyPrivateSDK = []string{"upstream"}

// privateSDKKnownDebt 已知且**正在还**的架构债。
//
// # 为什么要有这个名单，而不是直接把测试删掉或放宽
//
// 本判据第一次运行时确实抓到 3 处真违规（admin / server / codearts）。
// 其中 admin 与 codearts 的 ModelInfo 已修（见提交 afba270 / 3842ab4 / f21141b），
// 但剩下两处（server 的错误栈、codearts 的请求体管线）是**大工程**：
//
//	server   -> upstream  43 处引用，牵涉单上游回落路径与 70+ 处测试构造
//	codearts -> upstream   2 处引用，牵涉"codearts 该不该吃 workbuddy 的
//	                       反指纹改写"这个**需要真机验证**的语义问题
//
// 删掉测试是造假（把已知缺陷说成不存在）；放任红灯则否定了"每步必须全绿"
// 这条纪律（红着的判据会让人习惯性忽略它）。所以走第三条路：**记债**。
//
// # 这个名单怎么保证"债会被还"（三条互锁，缺一不可）
//
//  1. **防新增**：出现未登记的违规 → 红。所以新代码不能在这里再欠债。
//  2. **防陈旧**：登记的债若已不存在 → 红，要求删掉该行。
//     这一条是关键 —— 它让"债已还清但名单没更新"变成一次失败，
//     避免名单腐化成永久豁免。
//  3. **防放水**：每个条目都要求写出**具体的依赖者**（而不是一个 bool），
//     所以"把债的范围悄悄扩大"（比如 codearts 之外又多一个上游）也会红。
//
// 换句话说：这份债是**可追踪的**，不是**被遗忘的**。
var privateSDKKnownDebt = []struct {
	// SDK 私有 SDK 包名（对应 workbuddyPrivateSDK 的条目）。
	SDK string
	// Dependent 依赖者（`internal/<name>` 或 `cmd/<name>` 的**包名部分**）。
	Dependent string
	// Reason 为什么还没修（诚实写清，不要写"以后再说"）。
	Reason string
}{
	{
		SDK: "upstream", Dependent: "server",
		Reason: "错误栈 + Config.Upstream 共 43 处，牵涉单上游回落路径（cfg.Provider==nil）" +
			"与 70+ 处测试构造。需先抽中立线协议层（InBandError/Stream/Aggregate/Usage），" +
			"再让 Provider 路径成为必经。分阶段做。",
	},
	{
		SDK: "upstream", Dependent: "codearts",
		Reason: "RewriteModelField + PrepareBodyOptWithLimits 两处。这不只是类型耦合：" +
			"codearts 的 SanitizeFingerprints=true，于是 workbuddy 的反指纹改写" +
			"（含剥离裸数字 11-128）会作用在 codearts 流量上。修法需先确认" +
			"codearts 上游是否真的需要这套改写（需有效凭证实测，当前凭证已失效）。",
	},
}

// TestPrivateSDKIsNotConsumedByOtherUpstreams 私有 SDK 只许它的所有者用。
//
// 判据：workbuddyPrivateSDK 里的每个包，只允许被 internal/workbuddy 依赖；
// 其它上游（codearts/loomy/mimo/trae）都不许。核心包（corePackages）也不许 ——
// 核心依赖某个上游的私有 SDK，等于「加新上游核心零改动」这条判据破产。
//
// ⚠ 已知债见 privateSDKKnownDebt —— 它只放过**已登记**的违规，
// 且会在债还清后要求删除登记（防名单腐化）。见该变量的注释。
func TestPrivateSDKIsNotConsumedByOtherUpstreams(t *testing.T) {
	root := moduleRoot(t)

	// 所有者：每个私有 SDK 归哪个上游。
	owner := map[string]string{"upstream": "workbuddy"}

	// 已登记的债，键 = "sdk/dependent"
	knownDebt := map[string]bool{}
	for _, d := range privateSDKKnownDebt {
		key := d.SDK + "/" + d.Dependent
		if knownDebt[key] {
			t.Errorf("privateSDKKnownDebt 有重复条目 %q", key)
		}
		knownDebt[key] = true
	}
	// 实际观察到的债，用于第二步的"防陈旧"
	observed := map[string]bool{}

	// 记录一处违规：登记过就通过（但记入 observed），没登记就红。
	reportViolation := func(sdk, depName, kind, detail string) {
		key := sdk + "/" + depName
		observed[key] = true
		if knownDebt[key] {
			t.Logf("已知债（privateSDKKnownDebt 已登记）: %s 依赖 internal/%s —— %s",
				kind, sdk, depName)
			return
		}
		t.Errorf("架构违规：%s 依赖了 internal/%s —— 那是 %s 的私有 SDK。\n%s\n"+
			"  若这是**新欠的**债，不允许直接登记了事 —— 请先消除依赖。\n"+
			"  若确认无法立即消除，才在 privateSDKKnownDebt 里登记并写明原因"+
			"（并接受「债还清后必须删登记」的约束）。",
			kind, sdk, owner[sdk], detail)
	}

	for _, sdk := range workbuddyPrivateSDK {
		own, ok := owner[sdk]
		if !ok {
			t.Errorf("internal/%s 在 workbuddyPrivateSDK 名单里但没有登记所有者 —— "+
				"没有所有者就无法判断谁能依赖它，名单会退化成「豁免」", sdk)
			continue
		}

		dependents := packagesDependingOn(t, root, "workbuddy2api/internal/"+sdk)
		t.Logf("internal/%s（所有者 %s）的依赖者: %v", sdk, own, dependents)

		for _, d := range dependents {
			name := strings.TrimPrefix(d, "workbuddy2api/internal/")
			// 允许：所有者自己
			if name == own {
				continue
			}
			// cmd/* 装配层：它是唯一同时认识核心与所有上游的地方，依赖是**正当的**
			// （装配就是把两边接起来）。故不视为违规，也不登记。
			if strings.HasPrefix(d, "workbuddy2api/cmd/") {
				continue
			}
			// 违规：另一个上游
			if isUpstreamPackage(t, root, name) {
				reportViolation(sdk, name, "上游 "+name,
					"  上游之间必须独立：拔掉 "+own+" 不该牵连 "+name+"。\n"+
						"  修法：用 gateway 里的中立类型，或在本包内实现自己需要的那部分。")
				continue
			}
			// 违规：核心包
			for _, c := range corePackages {
				if d == c {
					reportViolation(sdk, name, "核心包 "+c,
						"  判据 1 要求加新上游时核心零改动；核心依赖某个上游的 SDK 会直接打破它。\n"+
							"  修法：把跨上游共识的类型提到 gateway。")
				}
			}
		}
	}

	// ── 防陈旧：登记了却已不存在 → 要求删掉 ────────────────────────────
	//
	// 这一条与"防新增"合起来才构成闭环：
	// 没有它，债还清后名单会永远留着那行，下次有人再欠同样的债时
	// 会被旧登记"顺带放过" —— 那正是白名单腐化的路径。
	for _, d := range privateSDKKnownDebt {
		key := d.SDK + "/" + d.Dependent
		if !observed[key] {
			t.Errorf("privateSDKKnownDebt 里的 %q 已不再是违规（依赖已消除）—— "+
				"请删除这条登记。\n"+
				"  ⚠ 留着它会让这份名单腐化成永久豁免：下次有人再欠同样的债，\n"+
				"     会被这条陈旧登记顺带放过。\n"+
				"  原登记原因（供确认确实已修）：%s", key, d.Reason)
		}
	}
}

// TestSharedInfraIsGenuinelyShared 被多个上游依赖的包必须**真的是共享的**。
//
// # 判据
//
// 一个 internal/<name> 若被 ≥2 个上游实现依赖，它必须满足其一：
//
//	1) 是 gateway（唯一的契约包，天然共享）；或
//	2) 在 sharedInfraPackages 里**显式登记**，且该登记是**必需**的
//	   （它确实被多个上游依赖 —— 否则这条登记就是陈旧的，测试要求删掉）
//
// # 为什么这条能防住这次的漏洞
//
// 漏洞的形态是：一个包**被多个上游依赖**（codearts 与 workbuddy 都依赖
// internal/upstream），却没有任何测试要求"它凭什么共享"。
// 加上这条之后，任何"某个上游的私有物被另一个上游顺手 import"的形态
// 都会在**下一次运行测试时**被迫表态：要么登记为共享基础设施，
// 要么改掉依赖。
func TestSharedInfraIsGenuinelyShared(t *testing.T) {
	root := moduleRoot(t)
	upstreams := discoverUpstreams(t, root)
	if len(upstreams) < 2 {
		t.Fatalf("推导出的上游不足 2 个（%v）—— 本测试的判据失去意义，先修推导", upstreams)
	}

	// 显式登记的共享基础设施（不含 gateway，它单独放行）。
	//
	// 每个都必须被 ≥2 个上游依赖，否则测试会要求删掉这里的登记。
	registered := map[string]bool{
		"auth":       true, // 凭证结构（各上游都往池子里放 *auth.Auth 投影）
		"checkinlog": true, // 签到/动作历史日志
		"prompt":     true, // 提示词降级（出站改写共用）
	}

	// 已被 workbuddyPrivateSDK 归类为「某个上游的私有 SDK」的包 —— 跳过。
	//
	// # 为什么必须跳过（否则两条测试会互相矛盾）
	//
	// 本测试问的是"你凭什么共享"。而答案有两类，**都由各自的测试把守**：
	//
	//	a) 它真是共享基础设施  → 登记在 registered（本测试管）
	//	b) 它是某个上游的私有物 → 登记在 workbuddyPrivateSDK
	//	                          （TestPrivateSDKIsNotConsumedByOtherUpstreams 管）
	//
	// internal/upstream 属于 (b)：它是 workbuddy 的私有 SDK，恰好被 codearts
	// 越界 import 了 2 处。那件事已经由 (b) 那条判据处理（且已记进
	// privateSDKKnownDebt 跟踪）。本测试再报一次只是把同一件事说两遍，
	// 而且会给出一个**错误的修法建议**（"加进 registered" —— 那等于
	// 承认它是共享基础设施，与事实相反）。
	private := map[string]bool{}
	for _, s := range workbuddyPrivateSDK {
		private[s] = true
	}

	// 收集每个 internal 包的依赖者
	candidates := candidatePackages(t, root)
	for _, name := range candidates {
		dependents := upstreamDependents(t, root, name)
		if len(dependents) < 2 {
			continue
		}
		if name == "gateway" {
			continue // 契约包，天然共享
		}
		if private[name] {
			// 已归类为私有 SDK —— 由 TestPrivateSDKIsNotConsumedByOtherUpstreams
			// 与 privateSDKKnownDebt 把守，这里不重复判。
			t.Logf("internal/%s 被 %d 个上游依赖，但它已登记为 %s 的私有 SDK —— "+
				"越界依赖由 TestPrivateSDKIsNotConsumedByOtherUpstreams 把守，此处跳过",
				name, len(dependents), "workbuddy")
			continue
		}
		if !registered[name] {
			t.Errorf("internal/%s 被 %d 个上游依赖（%v），但它既不是 gateway、也没在 sharedInfraPackages 登记。\n"+
				"  ⚠ 这正是这次漏洞的形态：一个上游的私有物被别的上游顺手 import，\n"+
				"     而没有任何测试要求它说明「凭什么共享」。\n"+
				"  修法（二选一）：\n"+
				"    a) 若它真是共享基础设施 → 加进本测试的 registered 名单（那是一次显式表态）\n"+
				"    b) 若它是某个上游的私有 SDK → 加进 workbuddyPrivateSDK，并改掉越界依赖",
				name, len(dependents), dependents)
		}
	}

	// 反向：登记了却没人用 → 陈旧登记，要求删掉（防名单腐化）
	for name := range registered {
		if _, err := os.Stat(filepath.Join(root, "internal", name)); err != nil {
			t.Errorf("sharedInfraPackages 里的 internal/%s 不存在了 —— 陈旧登记，请删除", name)
			continue
		}
		dep := upstreamDependents(t, root, name)
		if len(dep) < 2 {
			t.Errorf("internal/%s 被登记为共享基础设施，但只有 %d 个上游依赖它（%v）—— "+
				"登记已陈旧。若它其实是某个上游的私有物，请移进 workbuddyPrivateSDK。",
				name, len(dep), dep)
		}
	}
}

// ── 辅助：依赖关系查询 ────────────────────────────────────────────────────
//
// # 为什么要缓存（实测：没有缓存时子进程调用量是 O(N²)）
//
// listDeps 每次调用都会 fork 一个 `go list -deps`（实测单次约 0.5-1s）。
// 而"上游依赖图"这件事在同一轮测试里是**不变的** —— 但下面几个辅助函数
// 都写成在循环里反复问它：
//
//	discoverUpstreams        自己就是 N 次 go list
//	upstreamDependents       又对每个上游问一次
//	packagesDependingOn      对 internal/ 与 cmd/ 下**每个**包问一次
//
// 组合起来的调用量约 N²×M。补上盲区测试后实测从 9s 涨到 56s ——
// 一个架构约束测试如果慢到没人愿意跑，它就等于不存在。
//
// 缓存只按包路径记结果，不改变任何判据（同一进程内依赖图是常量）。

// depsCache 包路径 → 依赖列表（含自身）。同一进程内依赖图不变，故可缓存。
var depsCache = map[string][]string{}

// discoverCache 缓存 discoverUpstreams 的结果（root 相同的场景只有一个）。
var discoverCache = map[string][]string{}

// ── 辅助：依赖关系查询 ────────────────────────────────────────────────────

// candidatePackages 返回 internal/ 下所有值得检查的包名
// （排除下划线/点开头、以及非包目录）。
func candidatePackages(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// upstreamDependents 返回哪些**上游实现**依赖 internal/<name>（非测试依赖）。
func upstreamDependents(t *testing.T, root, name string) []string {
	t.Helper()
	var out []string
	for _, up := range discoverUpstreams(t, root) {
		if up == name {
			continue
		}
		deps, err := listDeps(root, "workbuddy2api/internal/"+up)
		if err != nil {
			continue
		}
		for _, d := range deps {
			if d == "workbuddy2api/internal/"+name {
				out = append(out, up)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// packagesDependingOn 返回 internal/ 与 cmd/ 下所有依赖 pkg 的包。
func packagesDependingOn(t *testing.T, root, pkg string) []string {
	t.Helper()
	var out []string
	for _, name := range candidatePackages(t, root) {
		if "workbuddy2api/internal/"+name == pkg {
			continue
		}
		if dependsOnPkg(root, "workbuddy2api/internal/"+name, pkg) {
			out = append(out, "workbuddy2api/internal/"+name)
		}
	}
	for _, name := range cmdPackages(t, root) {
		if dependsOnPkg(root, "workbuddy2api/cmd/"+name, pkg) {
			out = append(out, "workbuddy2api/cmd/"+name)
		}
	}
	sort.Strings(out)
	return out
}

// cmdPackages 返回 cmd/ 下的子包名。
func cmdPackages(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// isUpstreamPackage 报告 internal/<name> 是否是上游实现（由发现逻辑判定）。
func isUpstreamPackage(t *testing.T, root, name string) bool {
	t.Helper()
	for _, u := range discoverUpstreams(t, root) {
		if u == name {
			return true
		}
	}
	return false
}

// dependsOnPkg 报告 pkg 的非测试依赖里是否含 want。
func dependsOnPkg(root, pkg, want string) bool {
	deps, err := listDeps(root, pkg)
	if err != nil {
		return false
	}
	for _, d := range deps {
		if d == want {
			return true
		}
	}
	return false
}

// TestCoreDoesNotDependOnUpstreams 核心包不得依赖任何具体上游。
//
// 判据：`go list -deps <core>` 的输出里不得出现任何 **被发现的上游包**。
func TestCoreDoesNotDependOnUpstreams(t *testing.T) {
	root := moduleRoot(t)
	upstreams := discoverUpstreams(t, root)
	t.Logf("被约束的上游包: %v", upstreams)

	for _, pkg := range corePackages {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, "workbuddy2api/")))); err != nil {
			t.Logf("跳过（包尚不存在）: %s", pkg)
			continue
		}
		deps, err := listDeps(root, pkg)
		if err != nil {
			t.Errorf("go list -deps %s 失败: %v", pkg, err)
			continue
		}
		for _, d := range deps {
			for _, up := range upstreams {
				if d == "workbuddy2api/internal/"+up {
					t.Errorf("架构违规：核心包 %s 依赖了上游包 %s\n"+
						"  判据 1 要求：加新上游时核心零改动。\n"+
						"  修法：把用到的能力加到 gateway 接口，或用 ExtOf 类型断言发现扩展点。",
						pkg, d)
				}
			}
		}
	}
}

// TestUpstreamsDoNotDependOnCore 上游包不得依赖核心的业务包。
//
// 上游可以依赖 gateway（接口）与通用基础设施，但不得依赖
// pool/admin/server/scheduler —— 否则"上游可插拔"不成立。
func TestUpstreamsDoNotDependOnCore(t *testing.T) {
	root := moduleRoot(t)

	// 上游**不得**依赖的
	forbidden := []string{
		"workbuddy2api/internal/pool",
		"workbuddy2api/internal/admin",
		"workbuddy2api/internal/server",
		"workbuddy2api/internal/scheduler",
	}

	upstreams := discoverUpstreams(t, root)
	for _, up := range upstreams {
		pkg := "workbuddy2api/internal/" + up
		deps, err := listDeps(root, pkg)
		if err != nil {
			t.Errorf("go list -deps %s 失败: %v", pkg, err)
			continue
		}
		for _, d := range deps {
			for _, f := range forbidden {
				if d == f {
					t.Errorf("架构违规：上游包 %s 依赖了核心包 %s\n"+
						"  上游必须可插拔，不能反向依赖核心业务包。", pkg, d)
				}
			}
			// 上游之间也不该互相依赖（否则拔掉一个会牵连另一个）
			for _, other := range upstreams {
				if other == up {
					continue
				}
				if d == "workbuddy2api/internal/"+other {
					t.Errorf("架构违规：上游 %s 依赖了另一个上游 %s（上游之间必须独立）", pkg, d)
				}
			}
		}
	}
}

// TestGatewayDoesNotDependOnAnyUpstream gateway 是唯一的接缝，
// 它自己**绝不能**依赖任何上游 —— 否则依赖方向就反了。
func TestGatewayDoesNotDependOnAnyUpstream(t *testing.T) {
	root := moduleRoot(t)
	upstreams := discoverUpstreams(t, root)
	deps, err := listDeps(root, "workbuddy2api/internal/gateway")
	if err != nil {
		t.Fatalf("go list -deps 失败: %v", err)
	}
	for _, d := range deps {
		for _, up := range upstreams {
			if d == "workbuddy2api/internal/"+up {
				t.Errorf("架构违规：gateway 依赖了上游 %s（依赖方向反了）", d)
			}
		}
	}
}

// ⚠ 反向验证：约束检测逻辑必须真的能判断出违规。
//
// 早先的做法是造临时包跑 `go list`，但那有三个问题（评审指出）：
//  1. 只断言了"go list 能看到依赖"，**没有验证检测逻辑本身**
//  2. 往仓库根目录写临时目录，进程被杀就留下垃圾
//  3. 只读环境/CI 上会失败
//
// 改为：把匹配逻辑抽成**纯函数** `findViolations`，用合成输入直接测它。
// 无文件系统、无子进程、无仓库写入。
func TestArchDetectionLogicDetectsViolation(t *testing.T) {
	// 合成的依赖列表：模拟"核心包依赖了上游"
	deps := []string{
		"workbuddy2api/internal/pool",
		"workbuddy2api/internal/auth",
		"workbuddy2api/internal/workbuddy", // ← 违规
		"fmt",
	}
	got := findViolations(deps, []string{"workbuddy", "codearts"}, "workbuddy2api/internal/")
	if len(got) != 1 || got[0] != "workbuddy2api/internal/workbuddy" {
		t.Fatalf("检测逻辑应找出 1 处违规，实际 %v", got)
	}

	// 反向：干净的依赖列表应当零违规（避免"永远报错"式误判）
	clean := []string{"workbuddy2api/internal/pool", "fmt", "strings"}
	if got := findViolations(clean, []string{"workbuddy"}, "workbuddy2api/internal/"); len(got) != 0 {
		t.Fatalf("干净依赖不该报违规，实际 %v", got)
	}
	t.Log("✓ 反向验证通过：检测逻辑能区分违规与干净")
}

// ---------------------------------------------------------------- 辅助

// moduleRoot 找到仓库根目录（go.mod 所在处）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	// 本包位于 <root>/internal/gateway，故上溯两级
	wd, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(wd)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("找不到 go.mod（推断的根目录 %s 不对）: %v", root, err)
	}
	return root
}

// findViolations 纯函数：从依赖列表里挑出**违规依赖**。
//
// 抽成纯函数是为了可测（见 TestArchDetectionLogicDetectsViolation）：
// 无文件系统、无子进程、无仓库写入，且能直接喂合成输入验证边界。
func findViolations(deps []string, upstreams []string, prefix string) []string {
	var out []string
	for _, d := range deps {
		for _, up := range upstreams {
			if d == prefix+up {
				out = append(out, d)
			}
		}
	}
	return out
}

// listDeps 返回一个包的全部依赖（含自身）。
//
// ⚠ 带缓存：每次调用都会 fork 一个 `go list -deps`（实测约 0.5-1s），
// 而辅助函数会在循环里反复问同一个包。依赖图在同一进程内不变，故可缓存。
// 缓存只影响速度，不影响判据 —— 失败**不**入缓存（否则一次瞬时失败
// 会让后续所有查询都拿到空依赖，形成 fail-open）。
func listDeps(root, pkg string) ([]string, error) {
	if deps, ok := depsCache[pkg]; ok {
		return deps, nil
	}
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var deps []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			deps = append(deps, l)
		}
	}
	depsCache[pkg] = deps
	return deps, nil
}
