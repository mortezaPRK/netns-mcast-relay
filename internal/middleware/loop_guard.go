package middleware

import (
	"crypto/md5"
	"time"
)

// DefaultLoopGuardWindow is long enough to catch a relay copy returning over
// a shared network, but shorter than normal mDNS retransmission intervals.
const DefaultLoopGuardWindow = 250 * time.Millisecond

type loopKey struct {
	payload [16]byte
	groupIP [16]byte
	port    uint16
	zone    string
}

// LoopGuard drops copies already observed by the relay before they can fan out
// again. Process is called synchronously by the single broadcast goroutine.
// This placement closes the race left by destination-socket echo suppression.
type LoopGuard struct {
	window      time.Duration
	seen        map[loopKey]time.Time
	nextCleanup time.Time
}

func NewLoopGuard(window time.Duration) *LoopGuard {
	if window <= 0 {
		window = DefaultLoopGuardWindow
	}
	return &LoopGuard{window: window, seen: make(map[loopKey]time.Time)}
}

func (g *LoopGuard) Process(pkt *Packet) {
	now := time.Now()
	key := loopKeyForPacket(pkt)
	if expires, ok := g.seen[key]; ok && now.Before(expires) {
		pkt.Data = nil
		return
	}

	if g.nextCleanup.IsZero() || !now.Before(g.nextCleanup) {
		for candidate, candidateExpiry := range g.seen {
			if !now.Before(candidateExpiry) {
				delete(g.seen, candidate)
			}
		}
		g.nextCleanup = now.Add(g.window)
	}
	g.seen[key] = now.Add(g.window)
}

func (g *LoopGuard) Stop() {}

func loopKeyForPacket(pkt *Packet) loopKey {
	key := loopKey{payload: md5.Sum(pkt.Data)}
	if pkt.Group == nil {
		return key
	}
	if ip := pkt.Group.IP.To16(); ip != nil {
		copy(key.groupIP[:], ip)
	}
	if pkt.Group.Port >= 0 && pkt.Group.Port <= 65535 {
		key.port = uint16(pkt.Group.Port)
	}
	key.zone = pkt.Group.Zone
	return key
}
