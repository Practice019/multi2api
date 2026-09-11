// RED 测试：probeGrowth 内部的 5 个上游调用应当并发。
//
// 实测（单账号实例）：
//
//	完整探针 2402ms（5 个串行调用）
//	最慢单调用 1590ms（/tasks）
//	并发化后理论上限 = 最慢那一个 ≈ 1590ms，可拿回 812ms（34%）
//
// 这 812ms 是**每次刷新的地板耗时**（Task 5 之后账号已并发，所以它不再被摊薄），
// 因此值得单独优化。
//
// 判定方式：用可控延迟的 stub —— /tasks 睡 150ms、其余各睡 150ms。
//
//	串行 = 5 × 150 = 750ms
//	并发 = 约 150ms（受最慢支配）
//
// 用「显著小于串行」来判定，并断言 5 个端点都被调用过（不能为了快而漏调）。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// probeStub 记录每个 growth 端点被调用的次数，并各自睡固定时长。
type probeStub struct {
	delay time.Duration
	cur   atomic.Int32
	peak  atomic.Int32

	tasks, streak, energy, quota, lottery atomic.Int32
	// failEnergy 为真时 /energy 返回 500，用于验证「单个调用失败不影响整份快照」。
	failEnergy atomic.Bool
}

func (s *probeStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		switch r.URL.Path {
		case base + "/tasks":
			s.tasks.Add(1)
			ok(`{"tasks":[]}`)
		case base + "/streak":
			s.streak.Add(1)
			ok(`{"streak":{"days":1,"makeup_dates":[]},"makeup_cards":{"balance":0,"max":4},"redemption_status":{"remaining_days":1,"tiers":[]}}`)
		case base + "/energy":
			s.energy.Add(1)
			if s.failEnergy.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			ok(`{"balance":0}`)
		case base + "/buddy/quota":
			s.quota.Add(1)
			ok(`{"affordable":0,"balance":0,"cost_per_open":10,"max_open_count":5}`)
		case base + "/lottery/chances":
			s.lottery.Add(1)
			ok(`{"balance":0}`)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func newProbeHarness(t *testing.T, delay time.Duration) (*Scheduler, *probeStub) {
	t.Helper()
	pb := &probeStub{delay: delay}
	srv := httptest.NewServer(pb.handler())
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Nickname: "测试号"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	log := checkinlog.New(filepath.Join(t.TempDir(), "c.json"), 30)
	return New(Config{Pool: p, Upstream: up, Log: log}), pb
}

// TestProbeGrowthCallsAreConcurrent 5 个调用并发发起。
func TestProbeGrowthCallsAreConcurrent(t *testing.T) {
	const delay = 150 * time.Millisecond
	s, pb := newProbeHarness(t, delay)

	t0 := time.Now()
	s.RefreshGrowth(true, false)
	elapsed := time.Since(t0)

	serial := 5 * delay // 750ms
	if elapsed > serial/2 {
		t.Errorf("单账号探针耗时 %v，5 个串行调用需 %v；"+
			"超过一半说明调用之间仍是串行", elapsed, serial)
	}

	// 峰值应 > 1（真的并发了）
	if p := pb.peak.Load(); p < 2 {
		t.Errorf("并发峰值仅 %d，说明 5 个调用没有并发", p)
	}

	// 关键：不能为了快而漏调 —— 5 个端点各至少 1 次
	counts := map[string]int32{
		"tasks": pb.tasks.Load(), "streak": pb.streak.Load(),
		"energy": pb.energy.Load(), "quota": pb.quota.Load(),
		"lottery": pb.lottery.Load(),
	}
	for name, n := range counts {
		if n == 0 {
			t.Errorf("端点 %s 未被调用（并发化不能漏字段）", name)
		}
	}
}

// TestProbeGrowthPartialFailureStillYieldsSnapshot 任一调用失败不应让整份快照消失。
//
// 这是并发化最容易引入的回归：以前串行时某个 err 只影响那一个字段，
// 改成并发后如果写共享变量没加保护、或某个失败直接 return，就会丢掉全部数据。
func TestProbeGrowthPartialFailureStillYieldsSnapshot(t *testing.T) {
	s, pb := newProbeHarness(t, 5*time.Millisecond)
	// 让 /energy 失败（返回 500）—— 其它 4 个正常
	pb.failEnergy.Store(true)

	s.RefreshGrowth(true, false)
	snaps := s.GrowthSnapshots()
	if len(snaps) != 1 {
		t.Fatalf("应有 1 份快照，得到 %d", len(snaps))
	}
	// /tasks 成功 ⇒ 快照主体仍在（不是只有 error 的空壳）
	if snaps[0].Error != "" && snaps[0].TasksTotal == 0 {
		t.Errorf("单个端点失败导致整份快照丢失: Error=%q TasksTotal=%d",
			snaps[0].Error, snaps[0].TasksTotal)
	}
}
