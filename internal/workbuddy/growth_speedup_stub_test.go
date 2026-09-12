// mixedStub：/tasks 慢、其余快 —— 模拟真实上游的延迟分布。
// 单独一个文件是因为它和 probeStub 的延迟模型不同（后者所有端点同延迟）。
package workbuddy

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

type mixedStub struct {
	tasksDelay time.Duration
	otherDelay time.Duration

	cur   atomic.Int32
	peak  atomic.Int32
	tasks atomic.Int32
}

func (s *mixedStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := s.cur.Add(1)
		for {
			p := s.peak.Load()
			if c <= p || s.peak.CompareAndSwap(p, c) {
				break
			}
		}
		defer s.cur.Add(-1)

		ok := func(body string) { w.Write([]byte(`{"code":0,"msg":"OK","data":` + body + `}`)) }
		const base = "/v2/activity/growth"
		switch r.URL.Path {
		case base + "/tasks":
			s.tasks.Add(1)
			time.Sleep(s.tasksDelay)
			ok(`{"tasks":[]}`)
		case base + "/streak":
			time.Sleep(s.otherDelay)
			ok(`{"streak":{"days":1,"makeup_dates":[]},"makeup_cards":{"balance":0,"max":4},"redemption_status":{"remaining_days":1,"tiers":[]}}`)
		case base + "/energy":
			time.Sleep(s.otherDelay)
			ok(`{"balance":0}`)
		case base + "/buddy/quota":
			time.Sleep(s.otherDelay)
			ok(`{"affordable":0,"balance":0,"cost_per_open":10,"max_open_count":5}`)
		case base + "/lottery/chances":
			time.Sleep(s.otherDelay)
			ok(`{"balance":0}`)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func newMixedHarness(t *testing.T, sb *mixedStub, accounts int) *Provider {
	t.Helper()
	srv := stubServer(t, sb.handler())

	as := make([]*auth.Auth, 0, accounts)
	for i := 0; i < accounts; i++ {
		as = append(as, testAuthNamed(string(rune('a'+i))+"-uid", "账号"))
	}
	s, _ := newTestProvider(t, srv, as...)
	return s
}
