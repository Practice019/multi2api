package channelstore

import (
	"sync"
	"testing"
)

func TestPickKeyForcedRR(t *testing.T) {
	for _, mode := range []string{"", "random", "round_robin"} {
		t.Run(mode, func(t *testing.T) {
			s := &Store{rr: map[string]int{}, rand: newRand(1)}
			ch := &Channel{ID: "test", ChannelInfo: ChannelInfo{KeySelectionMode: mode}, Keys: []ChannelKey{{Key: "fake-a", Status: 1}, {Key: "fake-b", Status: 1}, {Key: "fake-c", Status: 1}}}
			for i := 0; i < 9; i++ {
				idx, _ := s.PickKeyIdx(ch)
				if idx != i%3 {
					t.Fatalf("pick %d: index %d want %d", i, idx, i%3)
				}
			}
			ch.Keys[0].Status = StatusManuallyDisabled
			idx, _ := s.PickKeyIdx(ch)
			if idx != 1 {
				t.Fatalf("disabled skip: %d", idx)
			}
			ch.Keys[1].Status = StatusAutoDisabled
			idx, _ = s.PickKeyIdx(ch)
			if idx != 2 {
				t.Fatalf("only enabled candidate: %d", idx)
			}
			ch.Keys[2].Key = ""
			if idx, key := s.PickKeyIdx(ch); idx != -1 || key != "" {
				t.Fatal("empty candidate returned")
			}
		})
	}
}

func TestPickKeyRRConcurrent(t *testing.T) {
	s := &Store{rand: newRand(1)}
	ch := &Channel{ID: "test", Keys: []ChannelKey{{Key: "fake-a", Status: 1}, {Key: "fake-b", Status: 1}, {Key: "fake-c", Status: 1}}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); idx, _ := s.PickKeyIdx(ch); mu.Lock(); counts[idx]++; mu.Unlock() }()
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		if counts[i] != 100 {
			t.Fatalf("counts: %v", counts)
		}
	}
}
