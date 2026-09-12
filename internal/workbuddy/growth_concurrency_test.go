// RED 测试：RefreshGrowth 必须并发执行（带上限），且结果与串行一致。
//
// 现场实测：3 账号 7157ms / 1 账号 2331ms，比值 3.07 —— 精确线性，
// 因为 RefreshGrowth 是纯串行 for 循环，而每账号要串行发 5 个上游请求。
// 10 个账号将是 ~23 秒。
//
// 本测试用"可控延迟的 stub"来验证并发：让每个 /tasks 各自睡 100ms，
// 串行 4 个账号要 ~400ms+，并发（上限 5）应显著更短。
package workbuddy

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// slowStub 每个请求睡 delay，并记录并发峰值。
type slowStub struct {
	delay  time.Duration
	cur    atomic.Int32
	peak   atomic.Int32
	tasksN atomic.Int32
}

func (s *slowStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 记录并发度
		c := s.cur.Add(1)
		for {
			p := s.peak.Load()
			if c <= p || s.peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(s.delay)
		s.cur.Add(-1)

		ok := func(body string) { w.Write([]byte(`{"code":0,"msg":"OK","data":` + body + `}`)) }
		const base = "/v2/activity/growth"
		switch {
		case r.URL.Path == base+"/tasks":
			s.tasksN.Add(1)
			ok(`{"tasks":[]}`)
		case r.URL.Path == base+"/streak":
			ok(`{"streak":{"days":1,"makeup_dates":[]},"makeup_cards":{"balance":0,"max":4},"redemption_status":{"remaining_days":1,"tiers":[]}}`)
		case r.URL.Path == base+"/energy":
			ok(`{"balance":0}`)
		case r.URL.Path == base+"/buddy/quota":
			ok(`{"affordable":0,"balance":0,"cost_per_open":10,"max_open_count":5}`)
		case r.URL.Path == base+"/lottery/chances":
			ok(`{"balance":0}`)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func newSlowHarness(t *testing.T, n int, delay time.Duration) (*Provider, *slowStub) {
	t.Helper()
	sb := &slowStub{delay: delay}
	srv := stubServer(t, sb.handler())

	accounts := make([]*auth.Auth, 0, n)
	for i := 0; i < n; i++ {
		accounts = append(accounts, testAuthNamed(string(rune('a'+i))+"-uid", "账号"))
	}
	s, _ := newTestProvider(t, srv, accounts...)
	return s, sb
}

// TestRefreshGrowthIsConcurrent 4 个账号、每账号 5 个请求、每个 100ms：
//
//	串行 = 4×5×100 = 2000ms
//	并发 = 上限 5 时约 5×100×(4/5) ≈ 400ms（数量级差异足够判定）
func TestRefreshGrowthIsConcurrent(t *testing.T) {
	const accounts = 4
	const delay = 100 * time.Millisecond
	s, sb := newSlowHarness(t, accounts, delay)

	t0 := time.Now()
	s.RefreshGrowth(true, false)
	elapsed := time.Since(t0)

	serial := time.Duration(accounts*5) * delay // 2000ms
	// 只要显著快于串行即可判定并发；给足余量避免 CI 抖动误报。
	if elapsed > serial/2 {
		t.Errorf("RefreshGrowth 耗时 %v，串行理论上限 %v；"+
			"超过一半说明没有并发（仍是串行 for 循环）", elapsed, serial)
	}
	if sb.tasksN.Load() != accounts {
		t.Errorf("应探测 %d 个账号，实际 tasks 被调 %d 次", accounts, sb.tasksN.Load())
	}
	// 并发上限 5：峰值不该超过 5
	if p := sb.peak.Load(); p > 5 {
		t.Errorf("并发峰值 %d 超过上限 5", p)
	}
}

// TestRefreshGrowthConcurrencyCap 10 个账号时峰值仍不超上限 5。
func TestRefreshGrowthConcurrencyCap(t *testing.T) {
	const accounts = 10
	s, sb := newSlowHarness(t, accounts, 40*time.Millisecond)

	s.RefreshGrowth(true, false)

	if p := sb.peak.Load(); p > 5 {
		t.Errorf("并发峰值 %d 超过上限 5（上游风控考虑，不能无脑全开）", p)
	}
	if sb.tasksN.Load() != accounts {
		t.Errorf("10 个账号都应被探测，实际 tasks 调用 %d 次", sb.tasksN.Load())
	}
	// 确实用了并发（峰值应 > 1）
	if p := sb.peak.Load(); p < 2 {
		t.Errorf("并发峰值仅 %d，说明没有真正并发", p)
	}
}

// TestRefreshGrowthResultsComplete 并发后每个账号都必须有快照，不能漏。
func TestRefreshGrowthResultsComplete(t *testing.T) {
	const accounts = 7
	s, _ := newSlowHarness(t, accounts, 10*time.Millisecond)

	s.RefreshGrowth(true, false)
	snaps := s.GrowthSnapshots()
	if len(snaps) != accounts {
		t.Errorf("应有 %d 份快照，实际 %d", accounts, len(snaps))
	}
	seen := map[string]bool{}
	for _, sn := range snaps {
		if sn.Error != "" {
			t.Errorf("账号 %s 快照有错误: %s", sn.UID, sn.Error)
		}
		seen[sn.UID] = true
	}
	if len(seen) != accounts {
		t.Errorf("快照 UID 不唯一或有缺失：%d 个", len(seen))
	}
}
