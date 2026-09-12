// gateway 契约与注册表的测试。
//
// 这是"工业化标准"的第一条地基：**新上游必须过一套契约测试才算合格**。
// 本文件先测 Registry 与契约检查器本身，并用**故意违规的假 Provider**
// 反向验证契约检查器真的能抓到问题（否则它只是个永远绿灯的摆设）。
package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 测试替身

type fakeStream struct {
	body   string
	closed bool
}

func (f *fakeStream) Read(p []byte) (int, error) {
	if f.body == "" {
		return 0, io.EOF
	}
	n := copy(p, f.body)
	f.body = f.body[n:]
	return n, nil
}
func (f *fakeStream) Close() error { f.closed = true; return nil }

// goodProvider 一个完全合规的实现，用来验证"合规的能通过"。
type goodProvider struct{ id string }

func (p *goodProvider) ID() string    { return p.id }
func (p *goodProvider) Caps() Capability { return CapChat | CapModels }
func (p *goodProvider) Chat(ctx context.Context, c Credential, body []byte) (ChatStream, error) {
	return ChatStream{Status: 200, Body: &fakeStream{body: "data: {}\n\n"}}, nil
}
func (p *goodProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m1", ContextWindow: 1000}}, nil
}

// ---------------------------------------------------------------- Registry

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	p := &goodProvider{id: "alpha"}
	if err := r.Register(p); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	got, ok := r.Get("alpha")
	if !ok || got.ID() != "alpha" {
		t.Fatalf("取回失败: %v %v", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("不存在的 id 不该返回 ok")
	}
}

// 重复注册必须报错，不能静默覆盖 —— 否则两个上游抢同一个 ID 时无法察觉。
func TestRegistryRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&goodProvider{id: "dup"}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(&goodProvider{id: "dup"})
	if err == nil {
		t.Fatal("重复注册应报错，实际 nil（会静默覆盖）")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("错误信息应含冲突的 id: %v", err)
	}
}

// 空 ID 必须在注册时就被拒绝（而不是运行时才发现路由不到）。
func TestRegistryRejectsEmptyID(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&goodProvider{id: ""}); err == nil {
		t.Fatal("空 ID 应报错")
	}
}

// nil Provider 不能让 Register panic。
func TestRegistryRejectsNil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("nil 应报错")
	}
}

// IDs 返回全部已注册 ID，顺序稳定（便于测试与展示）。
func TestRegistryIDsSorted(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"charlie", "alpha", "bravo"} {
		if err := r.Register(&goodProvider{id: id}); err != nil {
			t.Fatal(err)
		}
	}
	ids := r.IDs()
	want := []string{"alpha", "bravo", "charlie"}
	if len(ids) != len(want) {
		t.Fatalf("IDs()=%v want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("IDs()=%v want %v（顺序应稳定）", ids, want)
		}
	}
}

// ---------------------------------------------------------------- 契约检查器

// 合规的 Provider 必须通过契约。
func TestContractAcceptsGoodProvider(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		RunProviderContract(t, func() Provider { return &goodProvider{id: "ok"} })
	})
}

// ⚠ 反向验证：契约检查器必须**真的能抓到违规**。
//
// 没有这一段，契约测试就只是个永远绿灯的摆设 ——
// 那正是本任务要避免的"测试固化缺陷"。
func TestContractCatchesViolations(t *testing.T) {
	// 1. ID 为空
	t.Run("empty-id", func(t *testing.T) {
		st := &spyT{}
		RunProviderContract(st, func() Provider { return &goodProvider{id: ""} })
		if !st.failed {
			t.Error("空 ID 应被契约判为不合格")
		}
	})

	// 2. Chat panic
	t.Run("chat-panic", func(t *testing.T) {
		st := &spyT{}
		RunProviderContract(st, func() Provider { return &panicProvider{} })
		if !st.failed {
			t.Error("Chat panic 应被契约判为不合格")
		}
	})

	// 3. 流不 Close（连接泄漏）
	t.Run("unclosed-stream", func(t *testing.T) {
		st := &spyT{}
		RunProviderContract(st, func() Provider { return &leakyProvider{} })
		if !st.failed {
			t.Error("流未 Close 应被契约判为不合格")
		}
	})

	// 4. 声明了能力却返回错误（假声明）
	t.Run("false-capability", func(t *testing.T) {
		st := &spyT{}
		RunProviderContract(st, func() Provider { return &liarProvider{} })
		if !st.failed {
			t.Error("声明 CapModels 但 Models 报错，应被判为不合格")
		}
	})
}

type panicProvider struct{ goodProvider }

func (p *panicProvider) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	panic("boom")
}

type leakyProvider struct{ goodProvider }

func (p *leakyProvider) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{Status: 200, Body: &notClosing{strings.NewReader("x")}}, nil
}

type notClosing struct{ io.Reader }

func (n *notClosing) Close() error { return errors.New("拒绝关闭") }

type liarProvider struct{ goodProvider }

func (p *liarProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return nil, errors.New("我不支持但我说我支持")
}

// spyT 是一个假的 testing.T，用来观察契约检查器是否报错。
// 只实现契约用到的子集。
type spyT struct {
	failed bool
	msgs   []string
}

func (s *spyT) Helper() {}
func (s *spyT) Errorf(format string, args ...any) {
	s.failed = true
	s.msgs = append(s.msgs, format)
}
func (s *spyT) Error(args ...any) { s.failed = true }
func (s *spyT) Fatalf(format string, args ...any) {
	s.failed = true
	s.msgs = append(s.msgs, "FATAL: "+format)
	panic(fatalSentinel{})
}
func (s *spyT) Fatal(args ...any) { s.Fatalf("%v", args...) }
func (s *spyT) Run(name string, f func(t TB)) bool {
	sub := &spyT{}
	defer func() { recover() }()
	f(sub)
	if sub.failed {
		s.failed = true
	}
	return !sub.failed
}
func (s *spyT) Logf(string, ...any)   {}
func (s *spyT) Log(...any)            {}
func (s *spyT) Skip(...any)           {}
func (s *spyT) Skipf(string, ...any)  {}
func (s *spyT) Name() string          { return "spy" }
func (s *spyT) Cleanup(func())        {}

type fatalSentinel struct{}

// ---------------------------------------------------------------- 能力位

// Capability 是位标志：必须能自由组合与检测，且 String() 可读。
func TestCapabilityBitOps(t *testing.T) {
	c := CapChat | CapModels | CapCheckin
	for _, one := range []Capability{CapChat, CapModels, CapCheckin} {
		if c&one == 0 {
			t.Errorf("组合后应包含 %v", one)
		}
	}
	if c&CapWelfare != 0 {
		t.Error("未声明的能力不该被认为具备")
	}
	if s := String(CapChat); s == "" || s == "unknown" {
		t.Errorf("CapChat 的 String() 应有可读名字，得到 %q", s)
	}
	// 所有能力都应有名字（避免将来加了位却忘了给名字）
	all := []Capability{CapChat, CapModels, CapCheckin, CapGrowth, CapTravel, CapWelfare, CapQuotaProbe}
	for _, one := range all {
		if String(one) == "unknown" {
			t.Errorf("能力 %d 没有可读名字", one)
		}
	}
}
