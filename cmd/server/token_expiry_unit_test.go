// token_expiry_unit_test.go 过期时刻**单位**的跨上游守卫。
//
// # 守的是什么（用户实测报出来的）
//
// 用户看到 minimax 的 Token 过期时间显示「**20715653 天**」（≈56755 年），
// 问"这个 token 过期时间这么长没问题吗" —— 真相是我们把单位搞错了。
//
// 契约（`gateway.CredentialExpiryExt` 的文档原话）：
//
//	TokenExpiry 报告这份凭证的过期时刻（**Unix 秒**）
//
// 而 **zcode 与 minimax 都回了毫秒**（两个都是我写的，zcode 的注释里
// 甚至写着"本仓的约定是毫秒"—— 那是错的，见下）。本仓**7 个**上游是对的，
// 其中 4 个（raccoon / cline / lobsterai / qoder）都显式写了
// `time.UnixMilli(ms).Unix()`。
//
// # 为什么这需要一个跨上游的守卫，而不是各包一条
//
// 这个缺陷的形态是**"看起来不像 bug"**：1000 倍的时间戳读起来像
// "这个上游的 token 有效期特别长"，而不是像错误。所以它不会引发排查，
// 只会引发疑问 —— 用户就是被这个疑问带到我这里的。
//
// 各包一条测试能守住**已知的两个**上游，但守不住**下一个**新上游：
// 写的人（很可能又是照抄隔壁代码的我）照样会犯。所以守卫要落在
// 核心这一侧 —— 那里能看到**全部**上游，包括将来才加的那些。
package main

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fakeExpiryProvider 一个只用来喂过期时刻的最小上游。
//
// 它故意**不**做任何转换 —— 回什么值由测试决定，这样"上游回毫秒"
// 这个形态能被精确复现（而不是依赖某个真实上游的内部结构）。
type fakeExpiryProvider struct {
	at int64
	ok bool
}

func (f *fakeExpiryProvider) ID() string               { return "fake-expiry" }
func (f *fakeExpiryProvider) Caps() gateway.Capability { return gateway.CapChat }
func (f *fakeExpiryProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (f *fakeExpiryProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

// TokenExpiry 原样回测试给的值（**不做任何单位转换**）。
func (f *fakeExpiryProvider) TokenExpiry(gateway.Credential) (int64, bool) {
	return f.at, f.ok
}

// 编译期确认它真的满足核心会断言的两个接口。
var (
	_ gateway.Provider            = (*fakeExpiryProvider)(nil)
	_ gateway.CredentialExpiryExt = (*fakeExpiryProvider)(nil)
)

// newExpiryRegistry 造一个只注册了假上游的注册表。
func newExpiryRegistry(t *testing.T, f *fakeExpiryProvider) *gateway.Registry {
	t.Helper()
	reg := gateway.NewRegistry()
	if err := reg.Register(f); err != nil {
		t.Fatalf("注册假上游失败: %v", err)
	}
	return reg
}

// **秒**（正确单位）应当原样通过。
//
// 这条与下面那条成对：没有它，"把哨兵阈值写成 0"也能让上一条通过，
// 而那会把**所有**上游的过期时刻都吞成"未知"。
func TestLiveTokenExpiryAcceptsSeconds(t *testing.T) {
	want := time.Now().Add(2 * time.Hour).Unix() // 秒
	f := &fakeExpiryProvider{at: want, ok: true}
	reg := newExpiryRegistry(t, f)

	got, ok := liveTokenExpiry(reg, f.ID(), gateway.Credential{
		Provider: f.ID(), UID: "u1", Secret: struct{}{},
	})
	if !ok {
		t.Fatal("秒级时间戳应被接受（哨兵不该误伤合法值）")
	}
	if got != want {
		t.Errorf("应原样返回 %d，实际 %d", want, got)
	}
}

// **毫秒**（错误单位）必须被拦下，按"未知"处理。
//
// # 为什么是"按未知处理"而不是"自动除以 1000"
//
// 猜测性修正会掩盖上游的 bug：那个上游的注释/实现仍然是错的，
// 下次换个地方用同一个值就又会错。而且"自动修正"无法区分
// "写错了单位"与"上游真的给了一个远期时刻"。
//
// 显示 `—`（不知道）比显示一个 1000 倍错的数字好得多：
// 前者用户会去查，后者用户会信。
func TestLiveTokenExpiryRejectsMillis(t *testing.T) {
	// 2026 年某个时刻的**毫秒**表示 —— 正是用户看到的那个形态。
	millis := time.Now().Add(2 * time.Hour).UnixMilli()
	f := &fakeExpiryProvider{at: millis, ok: true}
	reg := newExpiryRegistry(t, f)

	got, ok := liveTokenExpiry(reg, f.ID(), gateway.Credential{
		Provider: f.ID(), UID: "u1", Secret: struct{}{},
	})
	if ok {
		t.Errorf("毫秒级时间戳 %d 必须被拦下 ——\n"+
			"它会让界面显示「%d 天」（≈%d 年），而用户读成"+
			"「这个 token 有效期特别长」而不是像 bug。",
			millis, (millis-time.Now().Unix())/86400, (millis-time.Now().Unix())/86400/365)
	}
	if got != 0 {
		t.Errorf("被拦下时应返回 0，实际 %d", got)
	}
}

// 边界：正好在阈值附近。
//
// 阈值取 1e11 秒（公元 5138 年）—— 真实凭证不可能有这么久的有效期，
// 而毫秒级时间戳恒 >= 1e12。两边的判据都要钉住，
// 否则有人把阈值调成 "1e12"（那会漏掉 1e11~1e12 之间的毫秒值）
// 或调成 "0"（那会吞掉全部合法值）都不会有测试变红。
func TestLiveTokenExpiryThresholdBoundary(t *testing.T) {
	cases := []struct {
		name string
		at   int64
		want bool
	}{
		{"典型秒级（2026 年）", 1793000000, true},
		{"远未来但仍是秒（公元 3000 年）", 32503680000, true},
		{"阈值下沿（1e11 - 1 秒）", 99_999_999_999, true},
		{"阈值本身（1e11 秒）", 100_000_000_000, false},
		{"典型毫秒（2026 年）", 1793000000000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeExpiryProvider{at: c.at, ok: true}
			reg := newExpiryRegistry(t, f)
			_, ok := liveTokenExpiry(reg, f.ID(), gateway.Credential{
				Provider: f.ID(), UID: "u1", Secret: struct{}{},
			})
			if ok != c.want {
				t.Errorf("%s: at=%d 应 %v，实际 %v", c.name, c.at, c.want, ok)
			}
		})
	}
}

// ok=false 与 at<=0 仍按"未知"处理（既有语义不能被这次改动碰坏）。
func TestLiveTokenExpiryUnknownPathsUnchanged(t *testing.T) {
	for _, c := range []struct {
		name string
		at   int64
		ok   bool
	}{
		{"ok=false", 1793000000, false},
		{"at=0", 0, true},
		{"at<0", -1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeExpiryProvider{at: c.at, ok: c.ok}
			reg := newExpiryRegistry(t, f)
			if _, ok := liveTokenExpiry(reg, f.ID(), gateway.Credential{
				Provider: f.ID(), UID: "u1", Secret: struct{}{},
			}); ok {
				t.Errorf("%s 应判未知", c.name)
			}
		})
	}
}
