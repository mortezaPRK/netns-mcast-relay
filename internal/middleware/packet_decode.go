package middleware

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const hexPreviewBytes = 64

var (
	mdnsIPv4 = net.ParseIP("224.0.0.251")
	mdnsIPv6 = net.ParseIP("ff02::fb")
	ssdpIPv4 = net.ParseIP("239.255.255.250")
)

func decodePacket(pkt *Packet) []slog.Attr {
	if pkt == nil || pkt.Group == nil {
		return unknownPacketAttrs(packetData(pkt))
	}

	switch {
	case pkt.Group.Port == 5353 && (pkt.Group.IP.Equal(mdnsIPv4) || pkt.Group.IP.Equal(mdnsIPv6)):
		attrs, err := decodeMDNS(pkt.Data)
		if err != nil {
			return decodeErrorAttrs("mdns", pkt.Data, err)
		}
		return attrs
	case pkt.Group.Port == 1900 && pkt.Group.IP.Equal(ssdpIPv4):
		attrs, err := decodeSSDP(pkt.Data)
		if err != nil {
			return decodeErrorAttrs("ssdp", pkt.Data, err)
		}
		return attrs
	default:
		return unknownPacketAttrs(pkt.Data)
	}
}

func packetData(pkt *Packet) []byte {
	if pkt == nil {
		return nil
	}
	return pkt.Data
}

func decodeMDNS(data []byte) ([]slog.Attr, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(data); err != nil {
		return nil, err
	}

	attrs := []slog.Attr{
		slog.String("protocol", "mdns"),
		slog.Uint64("id", uint64(msg.ID)),
		slog.Bool("response", msg.Response),
		slog.Int("questions", len(msg.Questions)),
		slog.Int("answers", len(msg.Answers)),
		slog.Int("authorities", len(msg.Authorities)),
		slog.Int("additionals", len(msg.Additionals)),
	}
	if len(msg.Questions) > 0 {
		q := msg.Questions[0]
		attrs = append(attrs,
			slog.String("question_name", q.Name.String()),
			slog.String("question_type", q.Type.String()),
		)
	}
	return attrs, nil
}

func decodeSSDP(data []byte) ([]slog.Attr, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("missing start line")
	}
	startLine := strings.TrimSpace(lines[0])
	if !validSSDPStartLine(startLine) {
		return nil, fmt.Errorf("invalid start line %q", startLine)
	}

	attrs := []slog.Attr{
		slog.String("protocol", "ssdp"),
	}
	parts := strings.Fields(startLine)
	if strings.HasPrefix(strings.ToUpper(startLine), "HTTP/") {
		attrs = append(attrs, slog.String("status", strings.Join(parts[1:], " ")))
	} else {
		attrs = append(attrs, slog.String("method", strings.ToUpper(parts[0])))
	}
	headers := make(textproto.MIMEHeader)
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid header %q", line)
		}
		headers.Add(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name)), strings.TrimSpace(value))
	}
	for _, name := range []string{"ST", "NT", "USN", "Location"} {
		if value := headers.Get(name); value != "" {
			attrs = append(attrs, slog.String(strings.ToLower(name), value))
		}
	}
	return attrs, nil
}

func validSSDPStartLine(line string) bool {
	upper := strings.ToUpper(line)
	return strings.HasPrefix(upper, "HTTP/1.") ||
		strings.HasSuffix(upper, " HTTP/1.0") ||
		strings.HasSuffix(upper, " HTTP/1.1")
}

func decodeErrorAttrs(protocol string, data []byte, err error) []slog.Attr {
	return []slog.Attr{
		slog.String("protocol", protocol),
		slog.String("error", err.Error()),
		slog.String("hex_preview", hexPreview(data)),
	}
}

func unknownPacketAttrs(data []byte) []slog.Attr {
	return []slog.Attr{
		slog.String("protocol", "unknown"),
		slog.String("hex_preview", hexPreview(data)),
	}
}

func hexPreview(data []byte) string {
	if len(data) > hexPreviewBytes {
		data = data[:hexPreviewBytes]
	}
	return hex.EncodeToString(data)
}
