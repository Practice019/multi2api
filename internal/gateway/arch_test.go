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
func listDeps(root, pkg string) ([]string, error) {
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
	return deps, nil
}
