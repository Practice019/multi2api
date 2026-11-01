package workbuddy

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// jwtWithLife 造一个只带 iat/exp 的 JWT（不签名；续期窗口只读时间戳）。
func jwtWithLife(iat, exp int64) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"iat":%d,"exp":%d}`, iat, exp)))
	return h + "." + p + ".sig"
}

// TestKeepaliveScheduleHonoursHalfLife 调度路径只刷"寿命过半"的账号，手动路径不受限。
//
// # 这条守的是 2026-11-01 的统一判据
//
// 改造前后台保活是**无条件全量续**：28~30 天寿命的 token 被每 30 分钟刷一次，
// 一天 48 次、一个周期约 1440 次，而它整个生命周期只需要一次。
// 用户要求「所有上游自动续期统一成生命周期过半就续期」之后，调度路径按比例拦，
// 但手动（管理台点"保活"）必须仍然无条件 —— 那是显式动作，跳过会让人以为按钮坏了。
func TestKeepaliveScheduleHonoursHalfLife(t *testing.T) {
	now := time.Now()

	// 寿命 30 天：刚签发的"新鲜"号 vs 已用 20 天（>50%）的"该刷"号。
	freshExp := now.Add(30 * 24 * time.Hour)
	fresh := &auth.Auth{
		UID: "fresh", RefreshToken: "rt", ExpiresAt: freshExp.Unix(),
		AccessToken: jwtWithLife(now.Unix(), freshExp.Unix()),
	}
	dueExp := now.Add(10 * 24 * time.Hour)
	due := &auth.Auth{
		UID: "due", RefreshToken: "rt", ExpiresAt: dueExp.Unix(),
		AccessToken: jwtWithLife(now.Add(-20*24*time.Hour).Unix(), dueExp.Unix()),
	}

	f := &fakeUpstream{}
	s, _ := newCheckinProvider(t, f, fresh, due)

	s.RunKeepaliveAll("schedule")
	if got := f.refreshCalls.Load(); got != 1 {
		t.Errorf("调度路径刷了 %d 个账号，want 1（只有已过半的那个）—— "+
			"新鲜号被刷说明比例门槛没生效（又回到无条件全量续了）", got)
	}
	if due.AccessToken != "new" {
		t.Error("已过半的账号必须被刷（它是这条判据存在的理由）")
	}

	// 手动：显式动作不受门槛限制（两个都刷）。
	s.RunKeepaliveAll("manual")
	if got := f.refreshCalls.Load(); got != 3 {
		t.Errorf("手动路径累计刷了 %d 个，want 3（手动那趟两个都刷）—— "+
			"手动被门槛拦住会让用户以为按钮坏了", got)
	}
}

// TestKeepaliveScheduleRefreshesWhenLifeUnreadable 读不出寿命的凭证**照旧刷**（不静默跳过）。
//
// 反例守卫：把这类凭证跳过看起来"更统一"，实际是删掉一条既有能力 ——
// 它们不再有任何机会被后台续期。保守方向必须是"该刷"而不是"跳过"。
func TestKeepaliveScheduleRefreshesWhenLifeUnreadable(t *testing.T) {
	// AccessToken 不是 JWT（手写/异常凭证），只有 ExpiresAt。
	a := &auth.Auth{UID: "opaque", AccessToken: "not-a-jwt", RefreshToken: "rt", ExpiresAt: time.Now().Add(90 * 24 * time.Hour).Unix()}
	f := &fakeUpstream{}
	s, _ := newCheckinProvider(t, f, a)
	s.RunKeepaliveAll("schedule")
	if got := f.refreshCalls.Load(); got != 1 {
		t.Errorf("读不出寿命时应当照旧刷（got=%d）—— 跳过等于让这类凭证永远不被续期", got)
	}
}
