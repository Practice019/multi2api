package gateway

import (
	"context"
	"errors"
	"testing"
)

// TestCapImportRequiresAccountImportExt 声明了 `CapImport` 就**必须**能导入。
//
// # 为什么这条必须有（假按钮防线）
//
// 前端的「批量导入」按钮判据是 `hasCap(pid,"import")` —— 能力位是**唯一**
// 的数据源。若某个上游声明了它却没实现 `AccountImportExt`，界面会渲染出
// 一个点下去回 501 的按钮：**按钮明明在那里，点了却说不支持**。
//
// 这与本仓既有的两条同一范式（"声明了就必须能用"）：
//
//	verifyAdminRoutes      声明了能力位就必须有对应管理端点
//	probeCapabilities      声明了 chat/models 就必须真的能用
//
// # 为什么用测试替身而不是遍历真实上游
//
// gateway 包**不认识任何具体上游**（架构硬约束：corePackages 不得 import
// 上游实现）。而"判据本身对不对"可以在这里用替身钉住：
//
//	不一致的替身 → 必须判失败
//	一致的替身   → 必须通过
//
// 真实上游那条由各自的契约测试跑（它们会调 RunProviderContract）。
// 这里钉的是**判据**，那边钉的是**被检查对象**——两者缺一不可：
// 判据写错时，真实的检查会变成装饰（本仓吃过这个亏，见
// isExtAccessor 的 NumIn() 注释）。
func TestCapImportRequiresAccountImportExt(t *testing.T) {
	if err := checkCapImportConsistency(&capImportInconsistent{}); err == nil {
		t.Fatal("声明了 CapImport 但没实现 AccountImportExt —— 应当判失败。" +
			"否则界面会渲染出一个点下去回 501 的假按钮")
	}
	if err := checkCapImportConsistency(&capImportConsistent{}); err != nil {
		t.Errorf("实现了 AccountImportExt 的正常上游不该报错: %v", err)
	}
	if err := checkCapImportConsistency(&capImportNeither{}); err != nil {
		t.Errorf("既没声明也没实现的上游不该报错（不支持导入是合法状态）: %v", err)
	}
}

// TestCapImportIsNamed 能力位必须有可读名。
//
// 少了名字时 `Capability.Names()` 会把它**静默丢弃** —— 于是 manifest 里
// 查不到 `import`，前端按钮永远不出现，而没有任何报错。
// 这正是 capNames 注释里那句"加能力时必须同时加这里 —— 有测试守住这一点"。
func TestCapImportIsNamed(t *testing.T) {
	if got := String(CapImport); got != "import" {
		t.Errorf("CapImport 的可读名 = %q，want \"import\" —— "+
			"没有名字时 Names() 会静默丢弃它，前端按钮永远不出现", got)
	}

	var inNames, inAll bool
	for _, n := range (CapChat | CapImport).Names() {
		if n == "import" {
			inNames = true
		}
	}
	for _, c := range AllCapabilities() {
		if c == CapImport {
			inAll = true
		}
	}
	if !inNames {
		t.Error("Names() 里没有 import —— 能力位会被静默丢弃")
	}
	if !inAll {
		t.Error("AllCapabilities() 里没有 CapImport —— 遍历全部能力位的测试会漏掉它")
	}
}

// TestSplitAccountImportItems 粘贴输入的拆分（数组 / 单对象 / 坏输入）。
//
// 这是**所有上游共用**的那一步（放 gateway 就是为了不抄 8 遍）。
// 判据：两种合法形状都认；空输入、空数组、非 JSON 都报**带原因的**错。
func TestSplitAccountImportItems(t *testing.T) {
	one := `{"a":1}`
	two := `[{"a":1},{"b":2}]`

	got, err := SplitAccountImportItems(one)
	if err != nil || len(got) != 1 {
		t.Errorf("单对象应当拆成 1 条，得到 %d 条 err=%v", len(got), err)
	}
	got, err = SplitAccountImportItems(two)
	if err != nil || len(got) != 2 {
		t.Errorf("数组应当拆成 2 条，得到 %d 条 err=%v", len(got), err)
	}
	// 带首尾空白的输入（用户复制粘贴常带换行）也要认
	if _, err := SplitAccountImportItems("\n  " + two + "\n  "); err != nil {
		t.Errorf("带空白前后缀的数组应当能解析: %v", err)
	}

	for _, bad := range []struct{ name, in string }{
		{"空串", ""},
		{"只有空白", "   \n  "},
		{"空数组", "[]"},
		{"非 JSON", "not json"},
		{"半截 JSON", `{"a":`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := SplitAccountImportItems(bad.in); err == nil {
				t.Errorf("%q 应当报错（静默返回空切片会让用户看到"+
					"「导入了 0 个」而不知道哪里错了）", bad.in)
			}
		})
	}
}

// ---- 判据本体 + 三个测试替身 ----

// errCapImportWithoutExt 声明了 CapImport 却没实现 AccountImportExt。
var errCapImportWithoutExt = errors.New(
	"声明了 CapImport 但未实现 AccountImportExt —— 界面会渲染出点下去报 501 的假按钮")

// checkCapImportConsistency 报告 p 的 CapImport 声明与扩展点实现是否一致。
func checkCapImportConsistency(p Provider) error {
	if p.Caps().Has(CapImport) {
		if _, ok := ExtOf[AccountImportExt](p); !ok {
			return errCapImportWithoutExt
		}
	}
	return nil
}

// capImportBase 满足 Provider 接口的最小实现（替身共用）。
type capImportBase struct{}

func (capImportBase) ID() string { return "stub" }
func (capImportBase) Caps() Capability {
	return CapChat
}
func (capImportBase) Chat(context.Context, Credential, []byte) (ChatStream, error) {
	return ChatStream{}, nil
}
func (capImportBase) Models(context.Context, Credential) ([]ModelInfo, error) {
	return nil, nil
}

// capImportInconsistent 声明了能力位但**没有**实现扩展点。
type capImportInconsistent struct{ capImportBase }

func (capImportInconsistent) Caps() Capability { return CapChat | CapImport }

// capImportConsistent 声明了能力位且实现了扩展点。
type capImportConsistent struct{ capImportBase }

func (capImportConsistent) Caps() Capability { return CapChat | CapImport }
func (capImportConsistent) ImportCredentials(string) ([]ImportedCredential, error) {
	return nil, nil
}

// capImportNeither 既不声明也不实现（不支持导入 —— 合法）。
type capImportNeither struct{ capImportBase }
