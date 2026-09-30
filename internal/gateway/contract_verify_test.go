// 独立验证：契约检查器是否**真的**能抓到违规。
//
// 为什么不信 gateway_test.go 里那组用例：那个 spyT 是我自己写的桩，
// 如果它的 Run/Fatalf 实现有 bug，"PASS" 可能是假的。
// 这里用**真实的 testing.T** 跑违规 Provider，断言它必须失败。
//
// 手法：用一个真实的 *testing.T 包一层，记录它有没有被 Errorf。
package gateway

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

// recordingT 包装真实 testing.T，只记录"有没有报错"，不真的让测试失败。
type recordingT struct {
	inner  *testing.T
	failed bool
	msgs   []string
}

func (r *recordingT) Helper() {}
func (r *recordingT) Errorf(f string, a ...any) {
	r.failed = true
	r.msgs = append(r.msgs, f)
}
func (r *recordingT) Fatalf(f string, a ...any) {
	r.failed = true
	r.msgs = append(r.msgs, "FATAL "+f)
	panic(fatalSentinel{})
}

// TestContractGenuinelyDetectsViolations 用真实 T 的包装器验证。
func TestContractGenuinelyDetectsViolations(t *testing.T) {
	cases := []struct {
		name string
		make func() Provider
		want string // 期望报错信息里出现的关键词（证明抓到的是**这一条**而不是别的）
	}{
		{"空ID", func() Provider { return &goodProvider{id: ""} }, "ID"},
		{"非法ID", func() Provider { return &goodProvider{id: "Bad_ID"} }, "ID"},
		{"大写ID", func() Provider { return &goodProvider{id: "Alpha"} }, "ID"},
		{"含斜杠ID", func() Provider { return &goodProvider{id: "a/b"} }, "ID"},
		{"缺CapChat", func() Provider { return &noChatCaps{} }, "CapChat"},
		{"Chat-panic", func() Provider { return &panicProvider{} }, "panic"},
		{"流不Close", func() Provider { return &leakyProvider{} }, "Close"},
		{"假声明Models", func() Provider { return &liarProvider{} }, "Models"},
		{"访问器式扩展点（ExtOf 看不见）", func() Provider {
			// 给一个合法 ID：否则会先报"ID() 为空"，掩盖真正要验的那条
			return &accessorOnlyProvider{goodProvider{id: "acc"}}
		}, "ExtOf"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recordingT{inner: t}
			func() {
				defer func() { recover() }() // 吞掉 Fatalf 的 sentinel
				RunProviderContract(rec, c.make)
			}()
			if !rec.failed {
				t.Fatalf("契约未抓到违规 %q —— 这个契约测试是摆设", c.name)
			}
			// 确认抓到的是**这一类**问题
			joined := strings.Join(rec.msgs, " | ")
			if !strings.Contains(joined, c.want) {
				t.Errorf("抓到了但报错信息不含 %q: %s", c.want, joined)
			}
			t.Logf("✓ %s → %s", c.name, firstLine(joined))
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '|'); i > 0 {
		return s[:i]
	}
	return s
}

// accessorOnlyProvider 复刻实测事故的形态：有 LoginFlow() 访问器，
// 但 **没有** 把 Start/Poll/Configured 挂到 *Provider 上。
//
// 这正是 cline / raccoon / lobsterai / qoder 四个上游原来的写法：
// 各自端到端测试走访问器（全绿），生产走 ExtOf 类型断言（必然失败）——
// 测试与生产走了两条不同的路。
type accessorOnlyProvider struct{ goodProvider }

func (a *accessorOnlyProvider) LoginFlow() (LoginFlow, bool) {
	return &fakeLoginFlow{}, true
}

// fakeLoginFlow 只为满足接口形状（本用例永远不会调到它）。
type fakeLoginFlow struct{}

func (f *fakeLoginFlow) Start() (string, string, error) { return "", "", nil }
func (f *fakeLoginFlow) Poll(string) (Credential, error) {
	return Credential{}, ErrLoginPending
}
func (f *fakeLoginFlow) Configured() bool { return true }

// TestExtAccessorPredicateIsNotSilentlyFalse 直接钉住反射判据本身。
//
// # 为什么必须单独测这个谓词
//
// 第一版 isExtAccessor 写成 `mt.NumIn() != 0` —— 但 reflect 的
// **方法** Func 类型把接收者算作第一个入参，于是 NumIn() 恒为 1，
// 谓词恒 false，整条 verifyExtensionsDiscoverable 静默失效：
// 四个有缺陷的上游照旧全绿。
//
// 也就是说"检查"自己踩了它要防的那个坑（判据写错 → 检查变成装饰）。
// 上面那条 accessorOnlyProvider 反向用例能抓到这种失效，
// 这里再把谓词的两侧边界直接钉住 —— 一个是行为级，一个是单元级。
func TestExtAccessorPredicateIsNotSilentlyFalse(t *testing.T) {
	// 正面：*Provider 上的 LoginFlow() 必须是"访问器"形态
	rt := reflect.TypeOf(&accessorOnlyProvider{})
	m, ok := rt.MethodByName("LoginFlow")
	if !ok {
		t.Fatal("测试桩自身有问题：找不到 LoginFlow 方法")
	}
	if !isExtAccessor(m.Type) {
		t.Errorf("isExtAccessor 未能认出 %v —— 谓词失效会让整条检查静默变绿", m.Type)
	}

	// 反面：业务方法（有入参/只有一个返回值）不得被误认成访问器，
	// 否则会把无关方法当成"声称实现了扩展点"而误报。
	if isExtAccessor(reflect.TypeOf(func(int) {})) {
		t.Error("有入参的函数不该被认成访问器")
	}
	if isExtAccessor(reflect.TypeOf(func() {})) {
		t.Error("无返回值的函数不该被认成访问器")
	}
	if isExtAccessor(reflect.TypeOf(func() (int, int) { return 0, 0 })) {
		t.Error("第二返回值非 bool 不该被认成访问器")
	}
}

// noChatCaps 声明了别的能力但没有 CapChat。
type noChatCaps struct{}

func (n *noChatCaps) ID() string       { return "nocap" }
func (n *noChatCaps) Caps() Capability { return CapModels }
func (n *noChatCaps) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{}, nil
}
func (n *noChatCaps) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "x"}}, nil
}

// TestContractAcceptsRealGoodProvider 确认合规实现**不会**被误判。
func TestContractAcceptsRealGoodProvider(t *testing.T) {
	rec := &recordingT{inner: t}
	func() {
		defer func() { recover() }()
		RunProviderContract(rec, func() Provider { return &goodProvider{id: "good"} })
	}()
	if rec.failed {
		t.Errorf("合规实现被误判为不合格: %v", rec.msgs)
	}
}

// TestSplitModel 模型名解析的边界（前缀路由的基础）。
func TestSplitModel(t *testing.T) {
	cases := []struct {
		in      string
		wantP   string
		wantM   string
		wantHas bool
	}{
		{"", "", "", false},
		{"auto", "", "auto", false},
		{"workbuddy/auto", "workbuddy", "auto", true},
		{"codearts/gpt-5.5", "codearts", "gpt-5.5", true},
		{"workbuddy/", "workbuddy", "", true},
		{"/auto", "", "/auto", false}, // 空前缀 → 不认，原样当裸名
		{"a/b/c", "a", "b/c", true},   // 只切第一个 /
		{"deepseek-v4-pro", "", "deepseek-v4-pro", false},
	}
	for _, c := range cases {
		p, m, has := SplitModel(c.in)
		if p != c.wantP || m != c.wantM || has != c.wantHas {
			t.Errorf("SplitModel(%q)=(%q,%q,%v) want (%q,%q,%v)",
				c.in, p, m, has, c.wantP, c.wantM, c.wantHas)
		}
	}
}

// TestExtOf 扩展点发现机制（平台特殊功能解耦的核心）。
func TestExtOf(t *testing.T) {
	// 实现了 AdminExt 的
	withExt := &extProvider{}
	if _, ok := ExtOf[AdminExt](withExt); !ok {
		t.Error("实现了 AdminExt 应能被发现")
	}
	// 没实现的
	plain := &goodProvider{id: "plain"}
	if _, ok := ExtOf[AdminExt](plain); ok {
		t.Error("没实现 AdminExt 不该被发现")
	}
	// nil 安全
	if _, ok := ExtOf[AdminExt](nil); ok {
		t.Error("nil Provider 不该 panic 或返回 true")
	}
}

// extProvider 一个实现了 AdminExt 的上游。
type extProvider struct{ goodProvider }

func (e *extProvider) AdminRoutes() []AdminRoute {
	return []AdminRoute{{Method: "GET", Path: "/admin/demo", Handler: nil}}
}

var _ io.Reader = (*strings.Reader)(nil)
