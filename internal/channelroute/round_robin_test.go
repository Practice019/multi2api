package channelroute

import (
	"sync"
	"testing"
)

func TestRoundRobinSequenceAndScopes(t *testing.T) {
	r := NewRoundRobin()
	ids := []string{"C", "A", "B", "A"}
	for _, want := range []string{"A", "B", "C", "A", "B", "C"} {
		if got, ok := r.Next("shared", ids, nil); !ok || got != want {
			t.Fatalf("got %q/%v want %q", got, ok, want)
		}
	}
	if got, _ := r.Next("other", ids, nil); got != "A" {
		t.Fatalf("independent scope got %q", got)
	}
	if ids[0] != "C" {
		t.Fatal("input mutated")
	}
}

func TestRoundRobinChangesAndExclusions(t *testing.T) {
	r := NewRoundRobin()
	r.Next("x", []string{"A", "B", "C"}, nil)
	r.Next("x", []string{"A", "B", "C"}, nil)
	if got, _ := r.Next("x", []string{"A", "C", "D"}, nil); got != "C" {
		t.Fatalf("deleted anchor got %q", got)
	}
	if got, _ := r.Next("x", []string{"A", "B", "C", "D"}, map[string]bool{"D": true}); got != "A" {
		t.Fatalf("excluded successor got %q", got)
	}
	if _, ok := r.Next("x", []string{"A"}, map[string]bool{"A": true}); ok {
		t.Fatal("all excluded")
	}
	if _, ok := r.Next("x", nil, nil); ok {
		t.Fatal("empty candidates")
	}
	if got, _ := r.Next("x", []string{"A", "B", "C"}, nil); got != "B" {
		t.Fatalf("failed selection moved cursor: %q", got)
	}
}

func TestRoundRobinConcurrentFairness(t *testing.T) {
	r := NewRoundRobin()
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, ok := r.Next("x", []string{"C", "A", "A", "B"}, nil)
			if !ok {
				t.Error("no candidate")
				return
			}
			mu.Lock()
			counts[id]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, id := range []string{"A", "B", "C"} {
		if counts[id] != 100 {
			t.Fatalf("unfair counts: %v", counts)
		}
	}
}
