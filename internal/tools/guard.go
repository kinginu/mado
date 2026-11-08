package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// ErrBlocked is returned for a target the SSRF guard refuses.
var ErrBlocked = errors.New("refused: the address is private, loopback or link-local")

// extra ranges that netip's predicates do not cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT, the tailnet
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("100::/64"),        // discard
}

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// Private reports whether ip is a target the guard refuses: loopback, link-local,
// RFC 1918, CGNAT, ULA, unspecified, multicast and the reserved ranges, also when
// written as an IPv4-mapped or NAT64 IPv6 address.
func Private(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	if !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Guard builds HTTP clients that only reach public addresses (DESIGN §17.7). The
// address is checked when the connection is dialled, after DNS resolution, so a
// redirect or a DNS answer that changes between check and use cannot get past it.
type Guard struct {
	AllowPrivate bool
	// Blocked overrides Private (tests).
	Blocked func(netip.Addr) bool
}

func (g Guard) blocked(ip netip.Addr) bool {
	if g.AllowPrivate {
		return false
	}
	if g.Blocked != nil {
		return g.Blocked(ip)
	}
	return Private(ip)
}

// CheckURL refuses a URL that is not http(s) or whose host is a refused literal
// address. Names are checked again at dial time.
func (g Guard) CheckURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http and https URLs can be fetched")
	}
	if u.Host == "" || u.User != nil {
		return fmt.Errorf("bad URL")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && g.blocked(ip) {
		return ErrBlocked
	}
	return nil
}

const maxRedirects = 5

// Client returns an http.Client with the dial-time check, no proxy (a proxy would
// do the dialling for us) and every redirect re-checked.
func (g Guard) Client(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var last error = ErrBlocked
			for _, ip := range ips {
				if g.blocked(ip) {
					continue
				}
				c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		return g.CheckURL(req.URL)
	}}
}
