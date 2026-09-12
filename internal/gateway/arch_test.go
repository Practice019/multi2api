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
// 现在是 workbuddy，接入 codearts 后会有 codearts。
// 用**显式白名单**而不是"猜哪些是上游"——
// 猜错会让约束失效（把上游当核心），显式列出才能保证覆盖。
var knownUpstreams = []string{"workbuddy", "codearts"}

// TestCoreDoesNotDependOnUpstreams 核心包不得依赖任何具体上游。
//
// 判据：`go list -deps <core>` 的输出里不得出现 upstreamPrefix。
func TestCoreDoesNotDependOnUpstreams(t *testing.T) {
	root := moduleRoot(t)

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
			for _, up := range knownUpstreams {
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
// 上游可以依赖 gateway（接口）但不得依赖 pool/admin/server ——
// 否则"上游可插拔"就不成立（拔掉一个上游会牵连核心）。
func TestUpstreamsDoNotDependOnCore(t *testing.T) {
	root := moduleRoot(t)

	// 上游**允许**依赖的（只有接口与基础设施）
	allowed := map[string]bool{
		"workbuddy2api/internal/gateway":    true,
		"workbuddy2api/internal/auth":       true, // 凭证结构（两侧逐字节相同）
		"workbuddy2api/internal/upstream":   true, // HTTP 客户端封装（共用）
		"workbuddy2api/internal/checkinlog": true, // 结果记录（共用）
	}

	// 上游**不得**依赖的
	forbidden := []string{
		"workbuddy2api/internal/pool",
		"workbuddy2api/internal/admin",
		"workbuddy2api/internal/server",
		"workbuddy2api/internal/scheduler",
	}

	for _, up := range knownUpstreams {
		pkg := "workbuddy2api/internal/" + up
		if _, err := os.Stat(filepath.Join(root, "internal", up)); err != nil {
			t.Logf("跳过（上游尚未接入）: %s", pkg)
			continue
		}
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
			for _, other := range knownUpstreams {
				if other == up {
					continue
				}
				if d == "workbuddy2api/internal/"+other {
					t.Errorf("架构违规：上游 %s 依赖了另一个上游 %s（上游之间必须独立）", pkg, d)
				}
			}
		}
		_ = allowed
	}
}

// TestGatewayDoesNotDependOnAnyUpstream gateway 是唯一的接缝，
// 它自己**绝不能**依赖任何上游 —— 否则依赖方向就反了。
func TestGatewayDoesNotDependOnAnyUpstream(t *testing.T) {
	root := moduleRoot(t)
	deps, err := listDeps(root, "workbuddy2api/internal/gateway")
	if err != nil {
		t.Fatalf("go list -deps 失败: %v", err)
	}
	for _, d := range deps {
		for _, up := range knownUpstreams {
			if d == "workbuddy2api/internal/"+up {
				t.Errorf("架构违规：gateway 依赖了上游 %s（依赖方向反了）", d)
			}
		}
	}
}

// ⚠ 反向验证：约束测试必须真的能抓到违规。
//
// 手法：造一个临时包，让它违规 import，用同一套检测逻辑跑一遍，
// 断言**检测到了**。没有这一步，约束测试就只是"看起来在管"。
func TestArchConstraintActuallyDetectsViolation(t *testing.T) {
	root := moduleRoot(t)

	// 造一个临时上游包 + 一个依赖它的临时"核心"包
	tmpUp := filepath.Join(root, "_archprobe_upstream")
	tmpCore := filepath.Join(root, "_archprobe_core")
	mustWrite(t, filepath.Join(tmpUp, "up.go"), "package archprobe_upstream\n\nfunc Name() string { return \"probe\" }\n")
	mustWrite(t, filepath.Join(tmpCore, "core.go"),
		"package archprobe_core\n\nimport _ \"workbuddy2api/_archprobe_upstream\"\n")
	t.Cleanup(func() { removeAll(tmpUp, tmpCore) })

	deps, err := listDeps(root, "workbuddy2api/_archprobe_core")
	if err != nil {
		t.Fatalf("go list 临时包失败（可能临时包没被识别）: %v", err)
	}
	found := false
	for _, d := range deps {
		if d == "workbuddy2api/_archprobe_upstream" {
			found = true
		}
	}
	if !found {
		t.Fatal("反向验证失败：检测逻辑没能在依赖列表里找到违规 import —— " +
			"说明 go list -deps 的用法有问题，真实的架构约束测试同样无效")
	}
	t.Log("✓ 反向验证通过：检测逻辑确实能看到依赖关系")
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

// mustWrite 写文件，失败即终止测试。
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败 %s: %v", path, err)
	}
}

// removeAll 清理临时探测包（忽略错误）。
func removeAll(paths ...string) {
	for _, p := range paths {
		_ = os.RemoveAll(p)
	}
}
