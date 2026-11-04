// channels_switch_timeout_test.go 钉住「候选渠道超时必须换人」这条契约。
//
// # 用户实测报障
//
// 「分组里 deepseek 的模型有很多上游，但现在只会用一个，不应该轮询吗？」
//
// 排查发现两条成因，本文件守的是第二条（可由代码修复的那条）：
//
//	① 客户端显式写 `upstream/model`（如 workbuddy/deepseek-v4.1-flash）时
//	   **本来就不轮询** —— 那是"指定上游"的语义，不是缺陷；
//	② 一对多公开名确实会分发，但候选里只要有一条卡死，`worthSwitching` 就把
//	   "单次尝试超时"误判成"整体已到期"，**于是不换人**，请求白白等满超时才失败。
//	   实测：5 次请求 3 败 2 成，其中一次干等 120 秒 —— 那 120 秒里同档其它渠道
//	   全程可用。
//
// 修法：判据从 `errors.Is(err, context.DeadlineExceeded)` 改为 `ctx.Err() != nil`
// （只有"请求上下文真的结束"才不换人）。
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/store"
)

// TestWorthSwitchingDistinguishesAttemptTimeoutFromRequestEnd 纯函数级契约。
func TestWorthSwitchingDistinguishesAttemptTimeoutFromRequestEnd(t *testing.T) {
	// ⚠ 必须是**包装过的** context.DeadlineExceeded（`%w`），不能用纯字符串：
	//   errors.Is 比对的是错误链，纯 errors.New("... context deadline exceeded ...")
	//   对 errors.Is(err, context.DeadlineExceeded) 恒为 false ——
	//   那样的用例在"旧判据"下也会通过，等于没有判别力（本用例第一版就踩了这个坑，
	//   是变异验证把它抓出来的）。
	attemptTimeout := fmt.Errorf("Post \"https://upstream/v1/chat\": %w", context.DeadlineExceeded)
	ctxAlive := context.Background()
	ctxCanceled, cancel := context.WithCancel(ctxAlive)
	cancel()
	ctxExpired, cancel2 := context.WithTimeout(ctxAlive, 0) // 立刻到期
	defer cancel2()
	<-ctxExpired.Done()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"单次尝试超时 + 请求上下文仍活着 ⇒ 该换人", ctxAlive, attemptTimeout, true},
		{"请求上下文已取消（客户端断开）⇒ 不换人", ctxCanceled, attemptTimeout, false},
		{"请求上下文已到期（网关整体预算用尽）⇒ 不换人", ctxExpired, attemptTimeout, false},
		{"上游普通 5xx ⇒ 该换人", ctxAlive, errors.New("upstream 502 bad gateway"), true},
		{"没有错误 ⇒ 无需求换人", ctxAlive, nil, false},
		{"组合根漏接线 ⇒ 换谁都一样", ctxAlive, errors.New("渠道模式已开启但没有注入 ChannelProvider 工厂（组合根漏接线）"), false},
	}
	for _, c := range cases {
		if got := worthSwitching(c.ctx, c.err); got != c.want {
			t.Errorf("%s：worthSwitching=%v want %v", c.name, got, c.want)
		}
	}
}

// ---- 行为级：同档两条候选，第一条超时、第二条健康 ⇒ 请求必须成功 ----

type timeoutProvider struct{ id string }

func (p *timeoutProvider) ID() string               { return p.id }
func (p *timeoutProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *timeoutProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (p *timeoutProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	// 与真实形态一致：http 客户端超时是**包装过的** DeadlineExceeded。
	return gateway.ChatStream{}, fmt.Errorf("Post \"https://upstream/v1/chat/completions\": %w", context.DeadlineExceeded)
}

type okProvider struct{ id string }

func (p *okProvider) ID() string               { return p.id }
func (p *okProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *okProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (p *okProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{Status: 200, Body: io.NopCloser(strings.NewReader("data: {\"ok\":true}\n\n"))}, nil
}

// TestForwardViaChannelSwitchesOnAttemptTimeout 卡死的候选不能拖死整个请求。
func TestForwardViaChannelSwitchesOnAttemptTimeout(t *testing.T) {
	s := timeoutSwitchStore(t)
	h := NewHandler(Config{
		Channels: s,
		// ch-dead 的 provider 永远超时；ch-alive 的永远成功。
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			if id == "ch-dead" {
				return &timeoutProvider{id: id}
			}
			return &okProvider{id: id}
		},
	})

	res, err := h.forwardViaChannel(context.Background(), "pub-z", []byte(`{"model":"pub-z"}`))
	if err != nil {
		t.Fatalf("同档里还有健康候选，请求不该失败（换人没生效）: %v", err)
	}
	if res.ChannelID != "ch-alive" {
		t.Errorf("应落到健康候选 ch-alive，实际 %s", res.ChannelID)
	}
}

// timeoutSwitchStore 两条同优先级候选：ch-dead（会超时）与 ch-alive（正常）。
//
// ⚠ 两条**同优先级**是刻意的：跨档降级是另一件事（attempt 递增），
//
//	本用例要钉的是"同档内换人"。
//
// ⚠ 两条渠道都声明 pub-z（各自的 ModelMap 指向不同上游名）——
//
//	这正是"一个公开名对应多个上游"的形态，也是用户报障的场景。
func timeoutSwitchStore(t *testing.T) *channelstore.Store {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	for _, c := range []struct{ id, name, up string }{
		{"ch-dead", "卡死的渠道", "real-dead"},
		{"ch-alive", "健康渠道", "real-alive"},
	} {
		if err := s.Upsert(&channelstore.Channel{
			ID: c.id, Name: c.name, BaseURL: "https://placeholder.invalid/v1",
			Status: channelstore.StatusEnabled, Priority: 10,
			Models:   []string{c.up},
			ModelMap: map[string]string{"pub-z": c.up},
			Keys:     []channelstore.ChannelKey{{Key: "sk-" + c.id, Status: channelstore.StatusEnabled}},
		}); err != nil {
			t.Fatalf("建渠道 %s 失败: %v", c.id, err)
		}
	}
	return s
}
