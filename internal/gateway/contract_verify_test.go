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
		{"空ID", func() Provider { return &goodProvider{id: "" } }, "ID"},
		{"非法ID", func() Provider { return &goodProvider{id: "Bad_ID" } }, "ID"},
		{"大写ID", func() Provider { return &goodProvider{id: "Alpha" } }, "ID"},
		{"含斜杠ID", func() Provider { return &goodProvider{id: "a/b" } }, "ID"},
		{"缺CapChat", func() Provider { return &noChatCaps{} }, "CapChat"},
		{"Chat-panic", func() Provider { return &panicProvider{} }, "panic"},
		{"流不Close", func() Provider { return &leakyProvider{} }, "Close"},
		{"假声明Models", func() Provider { return &liarProvider{} }, "Models"},
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

// noChatCaps 声明了别的能力但没有 CapChat。
type noChatCaps struct{}

func (n *noChatCaps) ID() string        { return "nocap" }
func (n *noChatCaps) Caps() Capability  { return CapModels }
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
		in       string
		wantP    string
		wantM    string
		wantHas  bool
	}{
		{"", "", "", false},
		{"auto", "", "auto", false},
		{"workbuddy/auto", "workbuddy", "auto", true},
		{"codearts/gpt-5.5", "codearts", "gpt-5.5", true},
		{"workbuddy/", "workbuddy", "", true},
		{"/auto", "", "/auto", false},          // 空前缀 → 不认，原样当裸名
		{"a/b/c", "a", "b/c", true},            // 只切第一个 /
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
