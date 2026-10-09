package gateway

import (
	"context"
	"errors"
	"strings"
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

// TestSplitAccountImportItemsMultipleObjects 多个**独立对象**连在一起也要认。
//
// # 这条守的是用户实测提的需求
//
// 用户原话："我通常导入的时候会导入多个独立的 JSON 文件，那这个时候它就不行了。
// 所以希望能够多加一个功能，就是如果它是多个独立的 JSON 文件，它可以自动
// 组合成一个完整的大的 JSON 文件，然后再导入。"
//
// 也就是他一次粘 N 个文件的内容。改造前 `SplitAccountImportItems` 只认
// 「单对象」与「[ ] 数组」两种形状 —— 而这个需求恰好落在两者之间：
// 用户手上是 N 个文件，不是一个大数组，所以**每次都会失败**。
//
// # 为什么"多个对象"要覆盖三种分隔形态
//
// 它们来自用户不同的复制手法，而每一种都在真实场景里出现过：
//
//	{…}\n{…}   依次换行粘贴（最常见）
//	{…}{…}     无分隔拼接（编辑器里连在一起）
//	{…},{…}    带逗号（从数组里复制掉方括号）
//
// 只覆盖一种会让用户"换个粘法又不行了"，而那是**无法自查**的失败。
func TestSplitAccountImportItemsMultipleObjects(t *testing.T) {
	// 故意用不同的键名，好断言"拆出来的顺序与内容都对"，
	// 而不只是"条数对"（条数对但内容串位是更隐蔽的错）。
	obj := func(k string) string { return `{"` + k + `":1}` }
	a, b, c := obj("a"), obj("b"), obj("c")

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"换行分隔（最常见：依次粘贴 N 个文件）",
			a + "\n" + b + "\n" + c, []string{a, b, c}},
		{"无分隔拼接", a + b, []string{a, b}},
		{"逗号分隔（从数组里复制掉方括号）", a + "," + b + "," + c, []string{a, b, c}},
		{"逗号 + 换行混用", a + ",\n" + b + ",\n  " + c, []string{a, b, c}},
		{"首尾带空白（复制粘贴常带）", "\n  " + a + "\n" + b + "\n  ", []string{a, b}},
		{"两个也算（不能只在 ≥3 时才生效）", a + "\n" + b, []string{a, b}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitAccountImportItems(tc.in)
			if err != nil {
				t.Fatalf("应当能拆开，却报错: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("拆出 %d 条，期望 %d 条: %#v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if strings.TrimSpace(got[i]) != tc.want[i] {
					t.Errorf("第 %d 条 = %q，期望 %q（顺序/内容串位比条数错更难发现）",
						i, strings.TrimSpace(got[i]), tc.want[i])
				}
			}
		})
	}
}

// 非对象形态的输入**绝不能**被"宽松改写"成一个条目。
//
// # 这条来自一次被变异验证抓出来的错误认知
//
// 我最初的注释写的是「宽松实现会把 `sk-abcdef` 变成 `["sk-abcdef"]`」——
// 那句话**是错的**：`[sk-abcdef]` 并不是合法 JSON（裸标识符），
// 所以那种实现根本不会碰它。我据此写的用例因此**测不出任何东西**
// （变异验证把"统一先包一层 [ ]"注进实现后，用例仍然全绿）。
//
// 真正会被宽松改写吃掉的是**本身就是合法 JSON 的值**：
//
//	"sk-abcdef"    带引号的字符串（从 JSON 文件里复制某个键的值）
//	123            数字
//	true           布尔
//
// 它们套上 [ ] 之后都是合法数组 → 会被当成"一个条目"交给上游，
// 而上游拿到的是个**被改写过的非对象**。所以这三类必须报错。
//
// 判据因此分两组：裸文本（括号后仍非法）与 JSON 原始值（括号后合法）——
// 只有后者能真正区分"保守"与"宽松"两种实现。
func TestSplitAccountImportItemsDoesNotWrapPrimitives(t *testing.T) {
	for _, in := range []string{
		// —— 裸文本：`[in]` 本身非法，保守实现自然拒绝 ——
		`sk-abcdef123456`,
		`zai:sk-abcdef123456`,
		`Bearer sk-abcdef123456`,
		`not json`,
		// —— JSON 原始值：`[in]` **合法**，只有保守实现才拒绝 ——
		//（这三条才是真正能区分两种实现的用例）
		`"sk-abcdef123456"`,
		`123`,
		`true`,
	} {
		t.Run(in, func(t *testing.T) {
			got, err := SplitAccountImportItems(in)
			if err == nil {
				t.Errorf("%q 不该被这个切分器接受（它是「逐条 JSON 对象」专用入口）——\n"+
					"实际拆出 %#v。\n"+
					"若实现是「统一先包一层 [ ] 再看」，这类**本身就是合法 JSON 的值**"+
					"会被包成数组、当成一个条目交给上游 —— 用户粘的东西被静默改写，\n"+
					"而上游拿到的是个非对象。", in, got)
			}
		})
	}
}

// 单个对象**不能被**这条新逻辑改变含义（回归防线）。
//
// 判据：`{…}` 仍然只拆出 1 条，且原样返回（不重新序列化 —— 上游按原文解析，
// 重新序列化会丢掉它可能依赖的键序/格式）。
func TestSplitAccountImportItemsSingleObjectUnchanged(t *testing.T) {
	one := `{  "a" : 1 , "b" : [1,2,3] }`
	got, err := SplitAccountImportItems(one)
	if err != nil {
		t.Fatalf("单个对象不该报错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("单个对象应拆成 1 条，实际 %d 条: %#v", len(got), got)
	}
	if got[0] != one {
		t.Errorf("单个对象应**原样**返回（不重新序列化），实际 %q", got[0])
	}
}

// ⚠ 尾部有残渣时必须**报错**，不能"能读几条算几条"。
//
// # 为什么这条最要紧
//
// 流式解析极易写成"读到一个算一个、读不动就停"。那样的话
//
//	{好对象}\n{好对象}\n{粘坏的
//
// 会变成"导入 2 个、第 3 个静默消失" —— 用户以为三个都进去了。
// 本项目的判据是**静默丢数据比明确失败糟得多**（见 gateway 里多处同款注释），
// 所以这里要求输入被完整消费。
func TestSplitAccountImportItemsRejectsTrailingGarbage(t *testing.T) {
	good := `{"a":1}`
	for _, bad := range []struct{ name, in string }{
		{"两个完好 + 一个半截", good + "\n" + `{"b":2}` + "\n" + `{"c":`},
		{"一个完好 + 尾部垃圾", good + "\n" + `这不是 JSON`},
		{"两个完好 + 尾部裸标识符", good + "\n" + `{"b":2}` + "\n" + `oops`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			got, err := SplitAccountImportItems(bad.in)
			if err == nil {
				t.Errorf("尾部有残渣时应当报错，实际静默拆出 %d 条: %#v\n"+
					"「能读几条算几条」会让用户以为全部导入成功，"+
					"而其中一部分**静默消失**", len(got), got)
			}
		})
	}
}

// 裸 API Key / 前缀形态**绝不能**被这条新逻辑改写。
//
// # 为什么这是独立的一条（而不是顺带的边界情况）
//
// 实现多对象支持最危险的写法是"统一先包一层 [ ] 再看能不能解析" ——
// 那会把 `sk-abcdef` 变成 `["sk-abcdef"]`，于是：
//
//	zcode 认裸 Key（`importOneKey`），而它拿到的是个**数组字面量字符串**；
//	上游可能仍然解析成功（把整串当 Key），也可能失败 —— 无论哪种，
//	**用户粘的东西被静默改了含义**，而这是最难查的一类缺陷。
//
// 所以判据是：不以 `{` / `[` 开头的输入，必须**原样报错**（而不是被包装）。
func TestSplitAccountImportItemsDoesNotWrapBareKeys(t *testing.T) {
	for _, bare := range []string{
		`sk-abcdef123456`,
		`zai:sk-abcdef123456`,
		`Bearer sk-abcdef123456`,
	} {
		t.Run(bare, func(t *testing.T) {
			got, err := SplitAccountImportItems(bare)
			if err == nil {
				t.Errorf("裸 Key %q 不该被这个切分器接受（它是 JSON 专用入口）——"+
					"实际拆出 %#v。\n若实现是「统一包一层 [ ]」，这里会得到 "+
					`["%s"] 这种被改写过的输入，让只认裸 Key 的上游（zcode）`+
					"静默改变含义", bare, got, bare)
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
