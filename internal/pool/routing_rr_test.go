package pool

import (
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func rrPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	t.Cleanup(func() { p.Close() })
	for _, uid := range []string{"C", "A", "B"} {
		p.AddFor("provider", &auth.Auth{UID: uid}, nil)
	}
	return p
}

func rrPick(t *testing.T, p *Pool, provider, model, want string, tried map[string]bool) {
	t.Helper()
	got := p.PickRoundRobinFor(provider, model, tried)
	if want == "" {
		if got != nil {
			p.Release(got.UID)
			t.Fatalf("expected no candidate, got %s", got.UID)
		}
		return
	}
	if got == nil {
		t.Fatalf("expected %s, got nil", want)
	}
	defer p.Release(got.UID)
	if got.UID != want {
		t.Fatalf("expected %s, got %s", want, got.UID)
	}
	p.mu.RLock()
	count := p.byUID[got.UID].inFlight.Load()
	p.mu.RUnlock()
	if count != 1 {
		t.Fatalf("selection must reserve once: %d", count)
	}
}

func TestRoutingRRSequenceChangesAndScopes(t *testing.T) {
	p := rrPool(t)
	for _, want := range []string{"A", "B", "C", "A"} {
		rrPick(t, p, "provider", "m", want, nil)
	}
	rrPick(t, p, "provider", "other", "A", nil)
	rrPick(t, p, "other", "m", "", nil)
	rrPick(t, p, "provider", "m", "C", map[string]bool{"B": true})
	p.Remove("C")
	p.AddFor("provider", &auth.Auth{UID: "D"}, nil)
	rrPick(t, p, "provider", "m", "D", nil)
	rrPick(t, p, "provider", "m", "", map[string]bool{"A": true, "B": true, "D": true})
	rrPick(t, p, "provider", "m", "A", nil)
}

func TestRoutingRRQualifications(t *testing.T) {
	p := rrPool(t)
	p.Disable("A", "test")
	p.Cooldown("B", CoolHard, time.Hour, "test")
	p.SetCredits("C", 0)
	rrPick(t, p, "provider", "m", "", nil)
	p.AddFor("provider", &auth.Auth{UID: "unknown"}, nil)
	rrPick(t, p, "provider", "m", "unknown", nil)
	p.mu.Lock()
	p.byUID["A"].disabled = false
	p.byUID["B"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()
	rrPick(t, p, "provider", "m", "A", nil)
	rrPick(t, p, "provider", "m", "B", nil)
	p.SetQuota("C", QuotaView{Kind: QuotaKindPerModel, HasData: true, ByModel: map[string]int64{"m": 0}})
	rrPick(t, p, "provider", "other", "A", nil)
	rrPick(t, p, "provider", "other", "B", nil)
	rrPick(t, p, "provider", "other", "C", nil)
}

func TestRoutingRRModelCooldownAndCapacity(t *testing.T) {
	p := rrPool(t)
	p.SetMaxInFlight(1)
	p.mu.Lock()
	p.byUID["A"].until = time.Now().Add(time.Hour)
	p.byUID["A"].softRateModel = "limited"
	p.byUID["B"].breakerUntil = time.Now().Add(time.Hour)
	p.mu.Unlock()
	rrPick(t, p, "provider", "limited", "C", nil)
	rrPick(t, p, "provider", "other", "A", nil)
	if !p.Acquire("C") {
		t.Fatal("legacy Acquire failed")
	}
	rrPick(t, p, "provider", "limited", "", nil)
	p.Release("C")
	rrPick(t, p, "provider", "limited", "C", nil)
}

func TestRoutingRRConcurrentReservations(t *testing.T) {
	p := rrPool(t)
	p.SetMaxInFlight(1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a := p.PickRoundRobinFor("provider", "m", nil); a != nil {
				mu.Lock()
				counts[a.UID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(counts) != 3 {
		t.Fatalf("expected 3 reserved accounts: %v", counts)
	}
	for uid, n := range counts {
		if n != 1 {
			t.Fatalf("duplicate reservation for %s: %d", uid, n)
		}
		p.Release(uid)
	}
	rrPick(t, p, "provider", "m", "A", nil)
}

func TestRoutingRRSharedScopeFairness(t *testing.T) {
	p := rrPool(t)
	for i := 0; i < 60; i++ {
		rrPick(t, p, "provider", "m", []string{"A", "B", "C"}[i%3], nil)
	}
}
