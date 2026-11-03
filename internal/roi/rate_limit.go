package roi

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}
type requestLimiter struct {
	mu      sync.Mutex
	clients map[string]tokenBucket
	account tokenBucket
	rate    float64
	burst   float64
	lastGC  time.Time
}

func newRequestLimiter(s SecurityConfig) *requestLimiter {
	return &requestLimiter{clients: map[string]tokenBucket{}, rate: float64(s.APIRequestsPerMinute) / 60, burst: float64(s.APIBurst)}
}
func (l *requestLimiter) take(b *tokenBucket, now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = l.burst
	} else {
		b.tokens = min(l.burst, b.tokens+max(0, now.Sub(b.last).Seconds())*l.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
func (l *requestLimiter) allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) >= time.Minute {
		for k, b := range l.clients {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.clients, k)
			}
		}
		l.lastGC = now
	}
	b, exists := l.clients[client]
	if !exists && len(l.clients) >= 10000 {
		return false
	}
	ok := l.take(&b, now)
	l.clients[client] = b
	return ok
}
func (l *requestLimiter) allowAccount(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.take(&l.account, now)
}
func rateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// A trusted proxy can be an exact IP or a CIDR. Normalize mapped IPv4 addresses
// consistently for configuration, socket peers and forwarding headers.
func proxyPrefix(raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if ip, err := netip.ParseAddr(raw); err == nil && ip.Zone() == "" {
		ip = ip.Unmap()
		return netip.PrefixFrom(ip, ip.BitLen()), nil
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid trusted proxy IP or CIDR %q", raw)
	}
	bits := p.Bits()
	ip := p.Addr()
	if ip.Is4In6() {
		bits -= 96
		ip = ip.Unmap()
	}
	if bits <= 0 {
		return netip.Prefix{}, fmt.Errorf("trusted proxy must not trust every address: %q", raw)
	}
	return netip.PrefixFrom(ip, bits).Masked(), nil
}
func (a *App) trustedProxy(ip netip.Addr) bool {
	for _, raw := range a.config.Security.TrustedProxies {
		p, err := proxyPrefix(raw)
		if err == nil && p.Contains(ip) {
			return true
		}
	}
	return false
}
func forwardedAddress(raw string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	return ip.Unmap(), err == nil && ip.Zone() == ""
}

func validateRealIPHeader(name string) error {
	if name != "" && (len(name) > 64 || !httpguts.ValidHeaderFieldName(name)) {
		return fmt.Errorf("security.real_ip_header must be an HTTP header name of at most 64 characters")
	}
	return nil
}

// Only a trusted immediate peer can introduce forwarded addresses. XFF takes
// precedence and is walked right to left up to the first untrusted hop. X-Real-IP
// is a single-address fallback only when XFF is absent, never when it is invalid.
func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	peer = peer.WithZone("").Unmap()
	if !a.trustedProxy(peer) {
		return peer.String()
	}
	configured := a.config.Security.RealIPHeader
	if configured != "" && !strings.EqualFold(configured, "X-Forwarded-For") {
		values := r.Header.Values(configured)
		if len(values) == 1 && len(values[0]) <= 256 {
			if ip, ok := forwardedAddress(values[0]); ok {
				return ip.String()
			}
		}
		return peer.String()
	}
	headers := r.Header.Values("X-Forwarded-For")
	if len(headers) == 0 {
		if configured != "" {
			return peer.String()
		}
		real := r.Header.Values("X-Real-IP")
		if len(real) == 1 && len(real[0]) <= 256 {
			if ip, ok := forwardedAddress(real[0]); ok {
				return ip.String()
			}
		}
		return peer.String()
	}
	// Repeated header lines are one ordered chain, not competing alternatives.
	if len(headers) > 20 {
		return peer.String()
	}
	size := len(headers) - 1
	for _, header := range headers {
		size += len(header)
		if size > 4096 {
			return peer.String()
		}
	}
	hops := strings.Split(strings.Join(headers, ","), ",")
	if len(hops) > 20 {
		return peer.String()
	}
	current := peer
	for i := len(hops) - 1; i >= 0; i-- {
		if !a.trustedProxy(current) {
			break
		}
		next, ok := forwardedAddress(hops[i])
		if !ok {
			return peer.String()
		}
		current = next
	}
	return current.String()
}
