package socket

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/mortezaPRK/netns-mcast-relay/internal/netns"
	"golang.org/x/net/ipv4"
)

const (
	// BufferSize is the socket buffer size (64KB)
	BufferSize = 64 * 1024
	// WriteTimeout bounds a single multicast send. A stuck interface errors
	// instead of blocking forever and starving other namespaces.
	WriteTimeout = 100 * time.Millisecond
	// ReadTimeout re-arms periodically so ReadFrom wakes to observe ctx cancellation.
	ReadTimeout = 1 * time.Second
	// selfEchoWindow bounds how long a just-written payload's hash is
	// remembered in order to recognize this socket's own IP_MULTICAST_LOOP
	// echo (see enableMulticastLoopback). Real loopback delivery completes
	// in well under a millisecond; this window is generous headroom against
	// scheduler jitter while staying far shorter than any realistic repeat
	// of the same query/response by an actual client, so it never mistakes
	// a second, independent request for the socket's own echo.
	selfEchoWindow = 500 * time.Millisecond
)

// MulticastSocket wraps a UDP multicast socket
type MulticastSocket struct {
	conn             *net.UDPConn
	packetConn       *ipv4.PacketConn
	rawConn          *ipv4.RawConn
	group            *net.UDPAddr
	interfaceIndex   int
	interfaceIndices []int
	logger           *slog.Logger

	// recentTx maps payload hash -> expiration time for self-echo suppression.
	// Written exclusively by single send worker, read by ReadFrom.
	recentTx atomic.Pointer[map[[16]byte]time.Time]
}

// NewMulticastSocket creates a multicast socket in the specified namespace
func NewMulticastSocket(group *net.UDPAddr, nsHandle *netns.NSHandle, logger *slog.Logger) (*MulticastSocket, error) {
	var (
		conn             *net.UDPConn
		packetConn       *ipv4.PacketConn
		rawConn          *ipv4.RawConn
		interfaceIndex   int
		interfaceIndices []int
	)

	logger.Debug("Creating multicast socket", "group", group)

	// Create the socket within the namespace
	if err := nsHandle.Do(func() error {
		// Find an appropriate interface for multicast
		ifaces, err := net.Interfaces()
		if err != nil {
			logger.Error("Failed to list interfaces", "error", err)
			return fmt.Errorf("failed to list interfaces: %w", err)
		}

		logger.Debug("Found interfaces", "count", len(ifaces))

		// Join every usable interface so multi-network namespaces do not depend
		// on kernel interface enumeration order.
		var usable []net.Interface
		for _, i := range ifaces {
			logger.Debug("Checking interface",
				"name", i.Name,
				"up", i.Flags&net.FlagUp != 0,
				"loopback", i.Flags&net.FlagLoopback != 0,
				"multicast", i.Flags&net.FlagMulticast != 0)

			if i.Flags&net.FlagUp != 0 && i.Flags&net.FlagLoopback == 0 && i.Flags&net.FlagMulticast != 0 {
				usable = append(usable, i)
			}
		}

		// If no suitable interface found, try loopback as fallback
		if len(usable) == 0 {
			logger.Debug("No non-loopback interface found, trying loopback")
			for _, i := range ifaces {
				if i.Flags&net.FlagUp != 0 && i.Flags&net.FlagLoopback != 0 {
					usable = append(usable, i)
					logger.Debug("Selected loopback interface", "name", i.Name)
					break
				}
			}
		}

		if len(usable) == 0 {
			logger.Error("No suitable network interface found")
			return fmt.Errorf("no suitable network interface found")
		}

		// ListenMulticastUDP automatically joins the multicast group
		primary := &usable[0]
		interfaceIndex = primary.Index
		interfaceIndices = make([]int, 0, len(usable))
		for i := range usable {
			interfaceIndices = append(interfaceIndices, usable[i].Index)
		}
		conn, err = net.ListenMulticastUDP("udp4", primary, group)
		if err != nil {
			logger.Error("Failed to create multicast socket",
				"group", group,
				"interface", primary.Name,
				"error", err)
			return fmt.Errorf("failed to create multicast socket for %s on interface %s: %w", group, primary.Name, err)
		}
		logger.Info("Created multicast socket", "group", group, "interface", primary.Name)

		packetConn = ipv4.NewPacketConn(conn)
		for i := 1; i < len(usable); i++ {
			iface := &usable[i]
			if err := packetConn.JoinGroup(iface, group); err != nil {
				_ = conn.Close()
				return fmt.Errorf("failed to join multicast group %s on interface %s: %w", group, iface.Name, err)
			}
			logger.Info("Joined multicast group", "group", group, "interface", iface.Name)
		}

		if err := enableMulticastLoopback(conn); err != nil {
			_ = conn.Close()
			logger.Error("Failed to enable multicast loopback", "interface", primary.Name, "error", err)
			return fmt.Errorf("failed to enable multicast loopback on interface %s: %w", primary.Name, err)
		}
		logger.Debug("Enabled multicast loopback", "interface", primary.Name)

		if err := packetConn.SetControlMessage(ipv4.FlagTTL|ipv4.FlagInterface, true); err != nil {
			_ = conn.Close()
			return fmt.Errorf("failed to enable multicast packet metadata: %w", err)
		}

		rawIPConn, rawErr := net.ListenIP("ip4:udp", &net.IPAddr{IP: net.IPv4zero})
		if rawErr != nil {
			logger.Warn("Source-preserving multicast unavailable; CAP_NET_RAW required", "error", rawErr)
		} else {
			rawConn, rawErr = ipv4.NewRawConn(rawIPConn)
			if rawErr != nil {
				_ = rawIPConn.Close()
				logger.Warn("Source-preserving multicast unavailable", "error", rawErr)
			} else if rawErr = rawConn.SetMulticastInterface(primary); rawErr != nil {
				_ = rawConn.Close()
				rawConn = nil
				logger.Warn("Source-preserving multicast unavailable", "error", rawErr)
			} else if rawErr = rawConn.SetMulticastLoopback(true); rawErr != nil {
				_ = rawConn.Close()
				rawConn = nil
				logger.Warn("Source-preserving multicast unavailable", "error", rawErr)
			}
		}

		// Set socket buffer sizes for better performance
		if err := conn.SetReadBuffer(BufferSize); err != nil {
			logger.Error("Failed to set read buffer", "error", err)
			if closeErr := conn.Close(); closeErr != nil {
				return fmt.Errorf("failed to set read buffer: %w (close error: %v)", err, closeErr)
			}
			return fmt.Errorf("failed to set read buffer: %w", err)
		}

		if err := conn.SetWriteBuffer(BufferSize); err != nil {
			logger.Error("Failed to set write buffer", "error", err)
			if closeErr := conn.Close(); closeErr != nil {
				return fmt.Errorf("failed to set write buffer: %w (close error: %v)", err, closeErr)
			}
			return fmt.Errorf("failed to set write buffer: %w", err)
		}
		logger.Debug("Set socket buffers", "size", BufferSize)

		return nil
	}); err != nil {
		return nil, err
	}

	return &MulticastSocket{
		conn:             conn,
		packetConn:       packetConn,
		rawConn:          rawConn,
		group:            group,
		interfaceIndex:   interfaceIndex,
		interfaceIndices: interfaceIndices,
		logger:           logger,
	}, nil
}

// enableMulticastLoopback makes relay writes visible to other sockets in the
// destination namespace. net.ListenMulticastUDP disables IP_MULTICAST_LOOP by
// default, which otherwise prevents local namespace processes from receiving
// packets forwarded by the relay.
func enableMulticastLoopback(conn *net.UDPConn) error {
	return ipv4.NewPacketConn(conn).SetMulticastLoopback(true)
}

// ReadFrom reads a packet from the multicast socket, transparently discarding
// this socket's own writes when IP_MULTICAST_LOOP hands them straight back
// (see enableMulticastLoopback and markSent/isSelfEcho). A read deadline is
// armed once per call so it still returns periodically for context
// cancellation even if nothing but self-echoes arrive before it.
func (s *MulticastSocket) ReadFrom(buf []byte) (int, *net.UDPAddr, int, error) {
	if err := s.conn.SetReadDeadline(time.Now().Add(ReadTimeout)); err != nil {
		s.logger.Error("Failed to set read deadline", "error", err)
		return 0, nil, 0, err
	}
	for {
		n, control, source, err := s.packetConn.ReadFrom(buf)
		if err != nil {
			// Don't log timeout errors at error level - they're expected
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				s.logger.Debug("Read timeout (expected for context checking)")
			} else {
				s.logger.Error("ReadFromUDP failed", "error", err)
			}
			return 0, nil, 0, err
		}
		addr, ok := source.(*net.UDPAddr)
		if !ok {
			return 0, nil, 0, fmt.Errorf("unexpected multicast source address type %T", source)
		}

		h := md5.Sum(buf[:n])
		s.logger.Debug("Received packet",
			"size", n,
			"from", addr,
			"hash", hex.EncodeToString(h[:4]))

		if s.isSelfEcho(buf[:n]) {
			s.logger.Debug("Dropped self-echo", "hash", hex.EncodeToString(h[:4]))
			continue
		}

		s.logger.Debug("Accepted packet", "hash", hex.EncodeToString(h[:4]))
		ttl := 0
		if control != nil {
			ttl = control.TTL
		}
		return n, addr, ttl, nil
	}
}

// WriteTo writes a packet to a specific address
func (s *MulticastSocket) WriteTo(data []byte, addr *net.UDPAddr) (int, error) {
	if err := s.conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		s.logger.Error("Failed to set write deadline", "error", err)
		return 0, err
	}
	n, err := s.conn.WriteToUDP(data, addr)
	if err != nil {
		s.logger.Error("WriteToUDP failed", "to", addr, "size", len(data), "error", err)
		return 0, err
	}
	s.logger.Debug("Wrote packet to address", "to", addr, "size", n)
	return n, nil
}

// WriteToGroup writes a packet to the multicast group.
// A write deadline bounds a stuck interface so it errors instead of starving
// other namespaces. The payload is marked as sent before it hits the wire so
// ReadFrom can recognize its own loopback echo the moment it comes back.
func (s *MulticastSocket) WriteToGroup(data []byte, source *net.UDPAddr, ttl int) (int, error) {
	h := md5.Sum(data)
	s.logger.Debug("Writing to group", "group", s.group, "size", len(data), "hash", hex.EncodeToString(h[:4]))

	s.markSent(data)

	if err := s.conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		s.logger.Error("Failed to set write deadline", "error", err)
		return 0, err
	}

	if ttl > 0 {
		if err := s.packetConn.SetMulticastTTL(ttl); err != nil {
			return 0, fmt.Errorf("set multicast TTL %d: %w", ttl, err)
		}
	}

	var (
		preserved bool
		writeErr  error
	)
	if source != nil && source.IP.To4() != nil && !source.IP.IsUnspecified() {
		if s.rawConn != nil {
			if _, err := s.writeRawUDP(data, source, ttl); err != nil {
				writeErr = errors.Join(writeErr, fmt.Errorf("source-preserving write: %w", err))
			} else {
				preserved = true
			}
		}
	}
	var sent int
	for _, interfaceIndex := range s.egressInterfaceIndices() {
		n, err := s.packetConn.WriteTo(data, &ipv4.ControlMessage{IfIndex: interfaceIndex}, s.group)
		if err != nil {
			writeErr = errors.Join(writeErr, err)
			continue
		}
		sent = n
	}
	if sent == 0 && !preserved {
		s.logger.Error("WriteToGroup failed", "group", s.group, "size", len(data), "error", writeErr)
		return 0, writeErr
	}
	if writeErr != nil {
		s.logger.Warn("WriteToGroup partially failed", "group", s.group, "size", len(data), "error", writeErr)
	}
	if sent == 0 {
		sent = len(data)
	}

	s.logger.Debug("Wrote to group", "group", s.group, "size", sent, "hash", hex.EncodeToString(h[:4]))
	return sent, nil
}

func (s *MulticastSocket) writeRawUDP(data []byte, source *net.UDPAddr, ttl int) (int, error) {
	if source.Port < 0 || source.Port > 65535 {
		return 0, fmt.Errorf("invalid UDP source port %d", source.Port)
	}
	if len(data) > 65507 {
		return 0, fmt.Errorf("UDP payload too large: %d", len(data))
	}
	if ttl <= 0 {
		ttl = 1
	}

	udpLength := 8 + len(data)
	payload := make([]byte, udpLength)
	binary.BigEndian.PutUint16(payload[0:2], uint16(source.Port))
	binary.BigEndian.PutUint16(payload[2:4], uint16(s.group.Port))
	binary.BigEndian.PutUint16(payload[4:6], uint16(udpLength))
	copy(payload[8:], data)
	binary.BigEndian.PutUint16(payload[6:8], udpChecksum(source.IP, s.group.IP, payload))

	header := &ipv4.Header{
		Version:  ipv4.Version,
		Len:      ipv4.HeaderLen,
		TotalLen: ipv4.HeaderLen + udpLength,
		TTL:      ttl,
		Protocol: 17,
		Src:      source.IP,
		Dst:      s.group.IP,
	}
	if err := s.rawConn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, err
	}
	var (
		sent     bool
		writeErr error
	)
	for _, interfaceIndex := range s.egressInterfaceIndices() {
		if err := s.rawConn.WriteTo(header, payload, &ipv4.ControlMessage{IfIndex: interfaceIndex}); err != nil {
			writeErr = errors.Join(writeErr, err)
			continue
		}
		sent = true
	}
	if !sent {
		return 0, writeErr
	}
	if writeErr != nil {
		s.logger.Warn("Raw multicast write partially failed", "group", s.group, "size", len(data), "error", writeErr)
	}
	return len(data), nil
}

func (s *MulticastSocket) egressInterfaceIndices() []int {
	if len(s.interfaceIndices) > 0 {
		return s.interfaceIndices
	}
	return []int{s.interfaceIndex}
}

func udpChecksum(source, destination net.IP, payload []byte) uint16 {
	source = source.To4()
	destination = destination.To4()
	var sum uint32
	for i := 0; i < net.IPv4len; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(source[i : i+2]))
		sum += uint32(binary.BigEndian.Uint16(destination[i : i+2]))
	}
	sum += 17
	sum += uint32(len(payload))
	for i := 0; i+1 < len(payload); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(payload[i : i+2]))
	}
	if len(payload)%2 != 0 {
		sum += uint32(payload[len(payload)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	checksum := ^uint16(sum)
	if checksum == 0 {
		return 0xffff
	}
	return checksum
}

// markSent records data's hash with expiration in the echo suppression map.
// Only the socket's single send-worker goroutine calls this, so load-modify-store never races.
func (s *MulticastSocket) markSent(data []byte) {
	now := time.Now()
	expires := now.Add(selfEchoWindow)
	h := md5.Sum(data)

	// Load existing map and clean expired entries
	live := make(map[[16]byte]time.Time)
	if prev := s.recentTx.Load(); prev != nil {
		for hash, exp := range *prev {
			if exp.After(now) {
				live[hash] = exp
			}
		}
	}

	// Add new entry
	live[h] = expires

	s.logger.Debug("Marked packet as sent",
		"hash", hex.EncodeToString(h[:4]),
		"expires", expires,
		"map_size", len(live))

	s.recentTx.Store(&live)
}

// isSelfEcho reports whether data matches something this socket wrote to the
// group within the last selfEchoWindow.
func (s *MulticastSocket) isSelfEcho(data []byte) bool {
	recent := s.recentTx.Load()
	if recent == nil {
		return false
	}

	now := time.Now()
	h := md5.Sum(data)

	if exp, ok := (*recent)[h]; ok {
		if exp.After(now) {
			s.logger.Debug("Self-echo detected",
				"hash", hex.EncodeToString(h[:4]),
				"expires", exp,
				"now", now)
			return true
		}
		s.logger.Debug("Hash found but expired",
			"hash", hex.EncodeToString(h[:4]),
			"expires", exp,
			"now", now)
	}

	return false
}

// Close closes the socket
func (s *MulticastSocket) Close() error {
	var rawErr error
	if s.rawConn != nil {
		rawErr = s.rawConn.Close()
	}
	if err := s.conn.Close(); err != nil {
		return err
	}
	return rawErr
}

// Group returns the multicast group address
func (s *MulticastSocket) Group() *net.UDPAddr {
	return s.group
}
