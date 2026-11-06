package server

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
)

// TrustedProxyConfig holds parsed CIDR networks used to determine whether
// the direct peer (RemoteAddr) is a trusted reverse proxy. When the peer is
// trusted, X-Forwarded-For and X-Real-Ip headers are used to extract the
// real client IP. When it is not, the headers are ignored and RemoteAddr is
// returned directly.
//
// A nil or empty TrustedProxyConfig trusts all peers (backwards-compatible
// with the previous behaviour).
type TrustedProxyConfig struct {
	nets []*net.IPNet
}

// NewTrustedProxyConfig parses a list of CIDR strings (e.g. "10.0.0.0/8",
// "172.16.0.0/12") into a TrustedProxyConfig. Single IPs without a mask
// (e.g. "10.0.0.1") are treated as /32 (IPv4) or /128 (IPv6).
// Returns an error if any entry is unparseable.
func NewTrustedProxyConfig(cidrs []string) (*TrustedProxyConfig, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, raw := range cidrs {
		cidr := strings.TrimSpace(raw)
		if cidr == "" {
			continue
		}
		// If there's no "/" it's a bare IP — wrap it as a host CIDR.
		if !strings.Contains(cidr, "/") {
			ip := net.ParseIP(cidr)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy IP: %q", cidr)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			cidr = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", raw, err)
		}
		nets = append(nets, ipNet)
	}
	if len(nets) == 0 {
		return nil, nil
	}
	return &TrustedProxyConfig{nets: nets}, nil
}

// IsTrusted returns true if addr (a "host:port" or bare IP string) falls
// within one of the configured trusted CIDR ranges. A nil config trusts
// all addresses (backwards-compatible default).
func (c *TrustedProxyConfig) IsTrusted(addr string) bool {
	if c == nil || len(c.nets) == 0 {
		return true
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		slog.Warn("trusted proxy check: unparseable remote addr", "addr", addr)
		return false
	}
	for _, n := range c.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
