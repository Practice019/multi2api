package channelroute

import (
	"sort"
	"sync"
)

type RoundRobin struct {
	mu   sync.Mutex
	last map[string]string
}

func NewRoundRobin() *RoundRobin {
	return &RoundRobin{last: make(map[string]string)}
}

func (r *RoundRobin) Next(scope string, ids []string, excluded map[string]bool) (string, bool) {
	candidates := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] || excluded[id] {
			continue
		}
		seen[id] = true
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.Strings(candidates)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = make(map[string]string)
	}
	index := sort.Search(len(candidates), func(i int) bool { return candidates[i] > r.last[scope] })
	if index == len(candidates) {
		index = 0
	}
	id := candidates[index]
	r.last[scope] = id
	return id, true
}
