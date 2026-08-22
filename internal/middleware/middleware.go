package middleware

import "net"

// Packet represents a multicast packet
type Packet struct {
	Data       []byte
	Source     *net.UDPAddr
	Group      *net.UDPAddr
	TTL        int
	SourcePath string // namespace path the packet arrived from (used to avoid echo)
}

// Middleware is an interface for packet processing components
type Middleware interface {
	// Process processes a packet in-place.
	// Set pkt.Data = nil to drop the packet.
	Process(pkt *Packet)
	// Stop terminates internal goroutines and releases middleware resources.
	Stop()
}

// Chain combines multiple middlewares into a pipeline
func Chain(middlewares ...Middleware) Middleware {
	return &chain{middlewares: middlewares}
}

type chain struct {
	middlewares []Middleware
}

// Process applies all middlewares in sequence, stopping early if packet is dropped
func (c *chain) Process(pkt *Packet) {
	for _, m := range c.middlewares {
		m.Process(pkt)
		if pkt.Data == nil {
			return // Already dropped, skip remaining middlewares
		}
	}
}

// Stop propagates stop to all middlewares in chain order.
func (c *chain) Stop() {
	for _, m := range c.middlewares {
		m.Stop()
	}
}
