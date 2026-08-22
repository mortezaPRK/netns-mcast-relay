package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mortezaPRK/netns-mcast-relay/internal/config"
	"github.com/mortezaPRK/netns-mcast-relay/internal/middleware"
	"github.com/mortezaPRK/netns-mcast-relay/internal/netns"
	"github.com/mortezaPRK/netns-mcast-relay/internal/socket"
)

const (
	// ChannelBuffer is the size of packet channels
	ChannelBuffer = 1000
	// DebounceWindow coalesces rapid add/remove storms into a single reconcile
	DebounceWindow = 300 * time.Millisecond
)

// nsSnapshot is an immutable view of active namespaces. The coordinator is the sole writer;
// it swaps a fresh map via atomic.Pointer. Readers Load() the pointer and iterate unlocked.
// This lock-free pattern outperforms RWMutex (blocks RLock on every I/O) and sync.Map for bulk
// iteration.
type nsSnapshot struct {
	instances map[string]*nsInstance
}

type groupAddrKey struct {
	ip      [16]byte
	port    int
	zone    string
	hasAddr bool
}

func groupKeyFromUDPAddr(addr *net.UDPAddr) groupAddrKey {
	var key groupAddrKey
	if addr == nil {
		return key
	}
	key.hasAddr = true
	key.port = addr.Port
	key.zone = addr.Zone
	if ip := addr.IP.To16(); ip != nil {
		copy(key.ip[:], ip)
	}
	return key
}

// nsInstance represents a single namespace with its sockets and send worker.
type nsInstance struct {
	path           string
	handle         *netns.NSHandle
	sockets        []*socket.MulticastSocket
	socketsByGroup map[groupAddrKey]*socket.MulticastSocket
	sendCh         chan *middleware.Packet // fed by broadcast(); drained by the send worker
	ctx            context.Context
	cancelFn       context.CancelFunc
	wg             sync.WaitGroup
}

// Relay manages multicast relay between dynamic namespaces.
type Relay struct {
	groups     []*net.UDPAddr
	middleware middleware.Middleware
	logger     *slog.Logger

	// snap is the read-heavy path: Load() only, never locked.
	snap atomic.Pointer[nsSnapshot]

	// Channel for namespace management commands (owned by coordinator goroutine).
	nsCmdCh chan *nsCmd

	// broadcastCh carries packets (with SourcePath set) from all receivers.
	broadcastCh chan *middleware.Packet

	// droppedEnqueue tracks dropped namespace enqueues due to full send queues.
	droppedEnqueue atomic.Uint64
}

type nsCmdType int

const (
	nsCmdAdd nsCmdType = iota
	nsCmdRemove
)

type nsCmd struct {
	op       nsCmdType
	path     string
	resultCh chan error
}

// New creates a new relay instance
func New(cfg *config.Config, logger *slog.Logger) (*Relay, error) {
	r := &Relay{
		groups:      cfg.Groups,
		middleware:  middleware.Chain(middleware.NewLoopGuard(middleware.DefaultLoopGuardWindow)),
		logger:      logger,
		nsCmdCh:     make(chan *nsCmd),
		broadcastCh: make(chan *middleware.Packet, ChannelBuffer),
	}
	r.snap.Store(&nsSnapshot{instances: make(map[string]*nsInstance)})

	logger.Info("Relay initialized", "groups", len(cfg.Groups))
	return r, nil
}

// Run starts the relay and blocks until context is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() {
		r.coordinate(ctx)
	})

	wg.Go(func() {
		r.broadcast(ctx)
	})

	<-ctx.Done()
	r.logger.Info("Relay shutting down...")

	// Stop middleware goroutines so Process() loop exits.
	r.middleware.Stop()
	wg.Wait()
	return nil
}

// AddNamespace records an intent to add a namespace. The actual application is
// coalesced by the coordinator's debounce window (fire-and-forget, like a burst
// gate). Returns nil on accept, not on applied.
func (r *Relay) AddNamespace(ctx context.Context, path string) error {
	resultCh := make(chan error, 1)
	select {
	case r.nsCmdCh <- &nsCmd{op: nsCmdAdd, path: path, resultCh: resultCh}:
		select {
		case err := <-resultCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RemoveNamespace records an intent to remove a namespace (debounced).
func (r *Relay) RemoveNamespace(ctx context.Context, path string) error {
	resultCh := make(chan error, 1)
	select {
	case r.nsCmdCh <- &nsCmd{op: nsCmdRemove, path: path, resultCh: resultCh}:
		select {
		case err := <-resultCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ActiveNamespaces returns a sorted snapshot of currently active namespace paths.
func (r *Relay) ActiveNamespaces() []string {
	snap := r.snap.Load()
	paths := make([]string, 0, len(snap.instances))
	for path := range snap.instances {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// coordinate owns namespace map and broadcast loop. It coalesces add/remove intents
// within DebounceWindow into one reconcile pass, turning update storms into single applies.
func (r *Relay) coordinate(ctx context.Context) {
	var (
		timer      *time.Timer
		timerCh    <-chan time.Time
		pendingAdd = map[string]bool{}
		pendingDel = map[string]bool{}
	)

	reconcile := func() {
		cur := r.snap.Load().instances
		next := make(map[string]*nsInstance, len(cur)+len(pendingAdd))
		for k, v := range cur {
			next[k] = v
		}
		// Remove first (teardown frees sockets/handles).
		for p := range pendingDel {
			if inst, ok := next[p]; ok {
				r.teardown(inst)
				delete(next, p)
			}
		}
		// Then add.
		for p := range pendingAdd {
			if _, ok := next[p]; !ok {
				if inst, err := r.buildInstance(ctx, p); err == nil {
					next[p] = inst
				} else {
					r.logger.Error("Failed to build namespace", "path", p, "error", err)
				}
			}
		}
		r.snap.Store(&nsSnapshot{instances: next})
		pendingAdd = map[string]bool{}
		pendingDel = map[string]bool{}
	}

	for {
		select {
		case cmd := <-r.nsCmdCh:
			switch cmd.op {
			case nsCmdAdd:
				pendingAdd[cmd.path] = true
				delete(pendingDel, cmd.path)
			case nsCmdRemove:
				pendingDel[cmd.path] = true
				delete(pendingAdd, cmd.path)
			}
			if timer == nil {
				timer = time.NewTimer(DebounceWindow)
				timerCh = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(DebounceWindow)
			}
			cmd.resultCh <- nil // intent accepted

		case <-timerCh:
			timer.Stop()
			timer = nil
			timerCh = nil
			reconcile()

		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			for _, inst := range r.snap.Load().instances {
				r.teardown(inst)
			}
			return
		}
	}
}

// broadcast reads packets, processes via middleware, and dispatches to every namespace
// except the source. It reads the namespace snapshot via Load() (no lock)
// and hands each packet to the destination's per-namespace send worker,
// which isolates slow/stuck interfaces to their own goroutine.
func (r *Relay) broadcast(ctx context.Context) {
	for {
		select {
		case pkt, ok := <-r.broadcastCh:
			if !ok {
				return
			}

			r.middleware.Process(pkt)
			if pkt.Data == nil {
				continue // Dropped by middleware
			}

			snap := r.snap.Load()
			for path, inst := range snap.instances {
				if path == pkt.SourcePath {
					continue // never echo back to source
				}
				if inst.ctx.Err() != nil {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case inst.sendCh <- pkt:
				default:
					dropped := r.droppedEnqueue.Add(1)
					if dropped == 1 || dropped%100 == 0 {
						r.logger.Warn("Namespace send queue full; dropping packet",
							"destination", inst.path,
							"group", pkt.Group,
							"dropped_total", dropped)
					}
				}
			}

		case <-ctx.Done():
			return
		}
	}
}

// buildInstance opens the namespace, creates sockets, and starts the receive
// goroutines plus the per-namespace send worker.
func (r *Relay) buildInstance(ctx context.Context, path string) (*nsInstance, error) {
	handle, err := netns.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open namespace: %w", err)
	}

	sockets := make([]*socket.MulticastSocket, 0, len(r.groups))
	socketsByGroup := make(map[groupAddrKey]*socket.MulticastSocket, len(r.groups))
	for _, group := range r.groups {
		socketLogger := r.logger.With("namespace", path, "group", group)
		sock, err := socket.NewMulticastSocket(group, handle, socketLogger)
		if err != nil {
			for _, s := range sockets {
				if closeErr := s.Close(); closeErr != nil {
					r.logger.Warn("Failed to close socket during rollback",
						"path", path,
						"group", s.Group(),
						"error", closeErr)
				}
			}
			if closeErr := handle.Close(); closeErr != nil {
				r.logger.Warn("Failed to close namespace handle during rollback",
					"path", path,
					"error", closeErr)
			}
			return nil, fmt.Errorf("failed to create socket for %s: %w", group, err)
		}
		sockets = append(sockets, sock)
		socketsByGroup[groupKeyFromUDPAddr(sock.Group())] = sock
	}

	nsCtx, cancelFn := context.WithCancel(ctx)
	inst := &nsInstance{
		path:           path,
		handle:         handle,
		sockets:        sockets,
		socketsByGroup: socketsByGroup,
		sendCh:         make(chan *middleware.Packet, ChannelBuffer),
		ctx:            nsCtx,
		cancelFn:       cancelFn,
	}

	for _, sock := range sockets {
		inst.wg.Go(func() {
			r.receive(nsCtx, sock, path, r.broadcastCh)
		})
	}
	r.startSendWorker(inst)

	r.logger.Info("Namespace added", "path", path, "groups", len(r.groups))
	return inst, nil
}

// teardown stops the receive + send goroutines and closes sockets/handle.
func (r *Relay) teardown(inst *nsInstance) {
	inst.cancelFn() // stops receive goroutines and the send worker
	for _, sock := range inst.sockets {
		if err := sock.Close(); err != nil {
			r.logger.Warn("Failed to close socket",
				"path", inst.path,
				"group", sock.Group(),
				"error", err)
		}
	}
	inst.wg.Wait()
	if err := inst.handle.Close(); err != nil {
		r.logger.Warn("Failed to close namespace handle",
			"path", inst.path,
			"error", err)
	}
	r.logger.Info("Namespace removed", "path", inst.path)
}

// startSendWorker runs one goroutine per namespace. It is the only place that
// writes to this namespace's sockets, so a stuck interface (bounded by the
// socket write deadline) degrades only this worker, never the broadcast loop or
// other namespaces.
func (r *Relay) startSendWorker(inst *nsInstance) {
	inst.wg.Go(func() {
		for {
			select {
			case pkt := <-inst.sendCh:
				if pkt == nil {
					continue
				}

				sock := inst.socketsByGroup[groupKeyFromUDPAddr(pkt.Group)]
				if sock == nil {
					continue
				}

				if _, err := sock.WriteToGroup(pkt.Data, pkt.Source, pkt.TTL); err != nil {
					if errors.Is(err, net.ErrClosed) {
						continue
					}
					r.logger.Warn("Error forwarding packet",
						"destination", inst.path,
						"group", pkt.Group,
						"error", err)
				}
			case <-inst.ctx.Done():
				return
			}
		}
	})
}

// receive reads packets from a socket and sends them (tagged with SourcePath)
// to the broadcast channel.
func (r *Relay) receive(ctx context.Context, sock *socket.MulticastSocket, sourcePath string, broadcastCh chan<- *middleware.Packet) {
	buf := make([]byte, 65535) // Max UDP packet size

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, srcAddr, ttl, err := sock.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Timeout (read deadline re-armed each call) or transient error:
			// wake and re-check context instead of blocking forever.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			r.logger.Warn("Error reading from socket",
				"namespace", sourcePath,
				"group", sock.Group(),
				"error", err)
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		pkt := &middleware.Packet{
			Data:       data,
			Source:     srcAddr,
			Group:      sock.Group(),
			TTL:        ttl,
			SourcePath: sourcePath,
		}

		select {
		case broadcastCh <- pkt:
		case <-ctx.Done():
			return
		}
	}
}
