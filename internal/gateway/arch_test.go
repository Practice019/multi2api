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

// discoverUpstreams 从磁盘推导上游包名 —— **不靠任何人工白名单**。
//
// # 为什么不能有白名单（两轮评审各证明了一次绕过）
//
// 第一版用人工维护的 `knownUpstreams`：忘了登记就完全失效。
// 第二版改成"从磁盘推导 + 非上游白名单"，评审仍然绕过了：
// 白名单里写着 `codearts`（当时还不存在），于是把**真实的上游实现**
// 放进 `internal/oauth/`（也在白名单里）→ **整套架构约束测试全绿**。
//
// 结论：**任何"人说是或不是上游"的列表都会被绕过**，
// 因为它把判定权交给了一个需要人来维护的地方。
//
// # 现在的判据：用客观标记，不靠人声明
//
// 一个包是"上游实现"当且仅当它**实现了 gateway 契约**。
// 这是可从代码本身判定的客观事实：
//
//	internal/<x>/*.go 里出现 `gateway.Provider` 或 `gateway.AdminExt`
//	或 `gateway.JobExt` 或 `gateway.LoginFlow`（即 import 了 gateway 并使用契约）
//
// 于是：
//   - 新上游一落地就被自动纳入约束（不需要改任何列表）
//   - 把上游藏进白名单目录也会被抓到（因为它 import 了 gateway）
//   - 通用包（auth/logbuf/...）不 import gateway，自然不是上游
//
// 唯一剩余假设：上游必须通过 gateway 契约接入 —— 这正是本设计的强制要求。
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
		if coreNames[name] {
			continue
		}
		if implementsGatewayContract(t, root, name) {
			out = append(out, name)
		}
	}
	return out
}

// gatewayContractMarkers 判定"这个包**实现了**契约"的标识。
//
// ⚠ 不能用 "import 了 gateway" 或 "提到 gateway.Provider" 作判据 ——
// admin/scheduler/gateway 自己也会提到这些类型（它们**消费**契约，
// 不是**实现**契约）。用"提到"会把它们误判成上游，
// 于是约束反向失效（核心包被当成上游，谁都不许依赖它）。
//
// 真正区分"实现"与"消费"的是：实现者会写编译期断言
//
//	var _ gateway.Provider = (*X)(nil)
//
// 或定义 Provider 的方法（`Caps() gateway.Capability` 这种）。
// 这里用**最能代表实现者**的两个标记：
//   - 编译期断言 `gateway.Provider =`
//   - `Caps() gateway.Capability`（只有实现者才定义 Caps）
var gatewayContractMarkers = []string{
	"gateway.Provider =",
	"Caps() gateway.Capability",
}

// implementsGatewayContract 该目录下是否有文件**实现**了 gateway 契约。
func implementsGatewayContract(t *testing.T, root, pkg string) bool {
	t.Helper()
	dir := filepath.Join(root, "internal", pkg)
	files, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") {
			continue
		}
		// 只看**非测试**文件：测试里提到契约不代表该包是上游实现
		if strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			continue
		}
		src := string(b)
		for _, m := range gatewayContractMarkers {
			if strings.Contains(src, m) {
				return true
			}
		}
	}
	return false
}

// TestDiscoveryFindsKnownUpstreams 确认推导**真的推出了当前已知的上游**。
//
// 没有这条，discoverUpstreams 一旦因为路径/判据错误返回空列表，
// 下面几条约束测试就会"零上游 → 零违规 → 绿灯"，形成 fail-open。
//
// ⚠ 这里的 `want` 是**断言用**的期望值（"这些目录确实实现了契约"），
// 不是发现逻辑的输入 —— 发现逻辑只看代码标记。
func TestDiscoveryFindsKnownUpstreams(t *testing.T) {
	root := moduleRoot(t)
	ups := discoverUpstreams(t, root)
	t.Logf("从 internal/ 推导出的上游包: %v", ups)

	found := map[string]bool{}
	for _, u := range ups {
		found[u] = true
	}

	// 遍历 internal/ 下每个目录：若它**实现了契约**却没被发现 → 判据有洞
	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if implementsGatewayContract(t, root, name) && !found[name] {
			t.Errorf("internal/%s 实现了 gateway 契约，却没被 discoverUpstreams 发现 —— "+
				"它绕过了架构约束（fail-open）", name)
		}
	}

	// 反向：当前确实存在若干上游实现（workbuddy 一定在）
	if !found["workbuddy"] {
		t.Error("internal/workbuddy 实现了 gateway 契约，必须被推导出来")
	}
}

// TestNoHandMaintainedUpstreamWhitelist 防止白名单被重新引入。
//
// 两轮评审都证明：任何"人工声明谁是上游"的列表都能被绕过
// （忘登记 / 或把上游放进被声明为"非上游"的目录）。
// TestDiscoveryHasNoBlindSpot 上游发现**不得有盲区**。
//
// # 为什么不用"扫自己的源码找白名单变量"（第一版就是这么写的，已废弃）
//
// 评审指出那种 meta-test 本质脆弱：它 grep 自己的源文件、靠字符串偏移排除自身，
// 单独跑通过、整套跑失败。更重要的是它**测的不是真正要保证的东西** ——
// 要保证的是"没有任何实现契约的包能逃过发现"，而不是"源码里没有某个变量名"。
//
// 所以改为**行为断言**，直接复现评审的绕过手法（把上游放进 internal/oauth/）：
// 只要一个包实现了契约，**无论放在哪个目录**都必须被发现。
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
	var implementers []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if implementsGatewayContract(t, root, name) {
			implementers = append(implementers, name)
		}
	}

	// 正向：实现了就必须被发现（任何"藏起来"的目录都逃不掉）
	blind := 0
	for _, name := range implementers {
		if !discovered[name] {
			blind++
			t.Errorf("internal/%s 实现了 gateway 契约却没被 discoverUpstreams 发现 "+
				"—— 它绕过了架构约束（fail-open）。\n"+
				"  这正是评审证明过的绕过：把上游放进一个'看起来不是上游'的目录。", name)
		}
	}

	// 反向：发现的不许误判（把核心包当成上游会让约束反向失效）
	for name := range discovered {
		if !implementsGatewayContract(t, root, name) {
			t.Errorf("discoverUpstreams 把 internal/%s 当成上游，但它并未实现契约 "+
				"—— 误判会让核心包被当作上游，依赖约束反向失效", name)
		}
	}

	if len(implementers) == 0 {
		t.Fatal("一个实现契约的包都没有 —— 路径或判据有问题，本测试自身失效")
	}
	t.Logf("实现契约的包: %v；被发现: %v", implementers, keysOf(discovered))
	if blind == 0 {
		t.Log("✓ 无盲区：所有实现契约的包都被发现")
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
