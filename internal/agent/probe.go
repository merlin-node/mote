package agent

import (
	"sync"
	"time"
)

type probeTracker struct {
	mu         sync.Mutex
	lastRTTMS  float64
	sent       int
	received   int
	lastPingAt time.Time
}

func newProbeTracker() *probeTracker {
	return &probeTracker{}
}

func (p *probeTracker) markSent(at time.Time) {
	p.mu.Lock()
	p.sent++
	p.lastPingAt = at
	p.mu.Unlock()
}

func (p *probeTracker) markPong(at time.Time) {
	p.mu.Lock()
	if !p.lastPingAt.IsZero() {
		p.lastRTTMS = float64(at.Sub(p.lastPingAt).Milliseconds())
	}
	p.received++
	p.mu.Unlock()
}

func (p *probeTracker) snapshotAndReset() (latencyMS, lossPct float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	latencyMS = p.lastRTTMS
	if p.sent > 0 {
		lost := p.sent - p.received
		if lost < 0 {
			lost = 0
		}
		lossPct = float64(lost) * 100 / float64(p.sent)
	}
	p.sent = 0
	p.received = 0
	return latencyMS, lossPct
}
