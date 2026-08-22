package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mortezaPRK/netns-mcast-relay/internal/netns"
)

// Config holds the relay configuration
type Config struct {
	InitialNamespaces  []string
	Groups             []*net.UDPAddr
	Verbose            bool
	ControlSocketPath  string
	ControlTimeout     time.Duration
	ControlSocketMode  os.FileMode
	ControlSocketGroup string
}

// groupsFlag is a custom flag type for multiple group specifications
type groupsFlag []string

func (g *groupsFlag) String() string {
	return strings.Join(*g, ",")
}

func (g *groupsFlag) Set(value string) error {
	*g = append(*g, value)
	return nil
}

// namespacesFlag is a custom flag type for multiple namespace specifications
type namespacesFlag []string

func (n *namespacesFlag) String() string {
	return strings.Join(*n, ",")
}

func (n *namespacesFlag) Set(value string) error {
	*n = append(*n, value)
	return nil
}

// Parse parses command line flags and returns a Config
func Parse() (*Config, error) {
	var groups groupsFlag
	var namespaces namespacesFlag

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	verbose := fs.Bool("verbose", false, "Enable debug logging")
	controlSocket := fs.String("control-socket", "", "Path to unix domain socket for runtime namespace control (optional)")
	controlTimeout := fs.Duration("control-timeout", 3*time.Second, "Timeout for control API operations")
	controlSocketMode := fs.String("control-socket-mode", "0600", "Permissions for the control socket (octal)")
	controlSocketGroup := fs.String("control-socket-group", "", "Group owner for the control socket (optional)")

	fs.Var(&groups, "group", "Multicast group address and port (e.g., 224.0.0.251:5353). Can be specified multiple times.")
	fs.Var(&namespaces, "ns", "Path to network namespace (e.g., /proc/1234/ns/net). Can be specified multiple times for initial namespaces.")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A lightweight multicast relay with dynamic namespace management.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nDefault groups (if none specified):\n")
		fmt.Fprintf(os.Stderr, "  mDNS: 224.0.0.251:5353\n")
		fmt.Fprintf(os.Stderr, "  SSDP: 239.255.255.250:1900\n")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  # Start with no namespaces (add dynamically later)\n")
		fmt.Fprintf(os.Stderr, "  %s --group=224.0.0.251:5353\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Start with 2 initial namespaces\n")
		fmt.Fprintf(os.Stderr, "  %s --ns=/proc/1234/ns/net --ns=/proc/5678/ns/net\n", os.Args[0])
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}

	canonicalNamespaces := make([]string, 0, len(namespaces))
	for _, nsPath := range namespaces {
		canonicalPath, err := canonicalizeStartupNamespacePath(nsPath)
		if err != nil {
			return nil, err
		}
		canonicalNamespaces = append(canonicalNamespaces, canonicalPath)
	}

	if *controlSocket != "" && !filepath.IsAbs(*controlSocket) {
		return nil, fmt.Errorf("control-socket path must be absolute: %s", *controlSocket)
	}
	if *controlTimeout <= 0 {
		return nil, fmt.Errorf("control-timeout must be positive")
	}
	parsedSocketMode, err := parseSocketMode(*controlSocketMode)
	if err != nil {
		return nil, err
	}

	// Parse multicast groups
	var groupAddrs []*net.UDPAddr
	if len(groups) == 0 {
		// Use default groups (mDNS and SSDP)
		groups = []string{"224.0.0.251:5353", "239.255.255.250:1900"}
	}

	for _, group := range groups {
		addr, err := parseMulticastGroup(group)
		if err != nil {
			return nil, fmt.Errorf("invalid multicast group %s: %w", group, err)
		}
		groupAddrs = append(groupAddrs, addr)
	}

	return &Config{
		InitialNamespaces:  canonicalNamespaces,
		Groups:             groupAddrs,
		Verbose:            *verbose,
		ControlSocketPath:  *controlSocket,
		ControlTimeout:     *controlTimeout,
		ControlSocketMode:  parsedSocketMode,
		ControlSocketGroup: strings.TrimSpace(*controlSocketGroup),
	}, nil
}

func parseSocketMode(value string) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(value, 8, 9)
	if err != nil || parsed == 0 || parsed > 0o777 {
		return 0, fmt.Errorf("control-socket-mode must be a non-zero octal permission mode: %s", value)
	}
	return os.FileMode(parsed), nil
}

func canonicalizeStartupNamespacePath(path string) (string, error) {
	cleaned, err := netns.CanonicalizePath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(cleaned); err != nil {
		return "", fmt.Errorf("namespace path %s does not exist: %w", cleaned, err)
	}
	return cleaned, nil
}

// parseMulticastGroup parses a multicast group string (e.g., "224.0.0.251:5353")
func parseMulticastGroup(group string) (*net.UDPAddr, error) {
	addr, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return nil, err
	}

	// Validate it's a multicast address (224.0.0.0/4)
	if !addr.IP.IsMulticast() {
		return nil, fmt.Errorf("%s is not a multicast address", addr.IP)
	}

	return addr, nil
}
