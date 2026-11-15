package pool

import (
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

func (p *Pool) PickRoundRobinFor(provider, model string, tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	provider = p.normalizeProvider(provider)
	ids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if e.a == nil || p.providerOf(e) != provider || tried[uid] || !e.healthyForModel(now, model) || p.inFlightFull(e) || p.creditsExhausted(e, model) {
			continue
		}
		ids = append(ids, uid)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	if p.rrLast == nil {
		p.rrLast = make(map[[2]string]string)
	}
	scope := [2]string{provider, model}
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > p.rrLast[scope] })
	for offset := 0; offset < len(ids); offset++ {
		uid := ids[(start+offset)%len(ids)]
		e := p.byUID[uid]
		for {
			count := e.inFlight.Load()
			if p.maxInFlight > 0 && count >= int64(p.maxInFlight) {
				break
			}
			if e.inFlight.CompareAndSwap(count, count+1) {
				p.rrLast[scope] = uid
				e.lastUsed = now
				p.pickSeq++
				e.lastPickSeq = p.pickSeq
				return e.a
			}
		}
	}
	return nil
}
