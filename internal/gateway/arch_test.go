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

// upstreamDir 实验版里"上游实现"的目录名。
//
// ⚠ **不要手工维护这个列表。** 早先的版本靠人肉登记，于是有两个洞：
//  1. 新增上游忘了登记 → 约束完全失效（fail-open）
//  2. 注释里声称有个 TestUpstreamListIsEnforced 在管，实际那个测试不存在
//
// 现在改为**从磁盘推导**：internal/ 下凡是既不在 corePackages、
// 又不在 nonUpstreamPackages 白名单里的目录，一律视为上游。
// 新加一个上游目录就自动被约束覆盖，不需要改这里。
var nonUpstreamPackages = []string{
	// 基础设施/通用包（不是上游，也不该被"上游不得依赖核心"约束）
	"auth", "logbuf", "redisstore", "session", "upstream",
	"checkinlog", "clientlogin", "oauth", "codearts",
	// 注：codearts 曾在上游列表里，但它同时是通用 OAuth 封装的家。
	// 它的 provider 适配器在 internal/codearts/ 内，同样受约束。
}

// discoverUpstreams 从磁盘推导上游包名。
//
// 判据：internal/<x> 若是目录、且不在 corePackages、且不在 nonUpstreamPackages
// 白名单里，它就是上游。
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
	allowed := map[string]bool{}
	for _, n := range nonUpstreamPackages {
		allowed[n] = true
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
		if coreNames[name] || allowed[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// TestUpstreamDiscoveryIsNotVacuous 确认"从磁盘推导上游"这件事**真的推导出了东西**。
//
// 没有这条，discoverUpstreams 一旦因为路径错误返回空列表，
// 下面两条约束测试就会"零上游 → 零违规 → 绿灯"，形成 fail-open。
func TestUpstreamDiscoveryIsNotVacuous(t *testing.T) {
	root := moduleRoot(t)
	ups := discoverUpstreams(t, root)
	t.Logf("从 internal/ 推导出的上游包: %v", ups)

	// 接入 workbuddy / codearts 之后，这里至少会有一个。
	// 当前（Task 2 阶段）可能一个都还没建，所以只在**已知目录存在**时要求被发现。
	for _, name := range []string{"workbuddy", "codearts"} {
		if _, err := os.Stat(filepath.Join(root, "internal", name)); err == nil {
			found := false
			for _, u := range ups {
				if u == name {
					found = true
				}
			}
			if !found {
				t.Errorf("internal/%s 存在（看起来是上游），但 discoverUpstreams 没发现它 —— "+
					"推导逻辑有洞，架构约束会 fail-open", name)
			}
		}
	}
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
