package roi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type hostState struct {
	reason         string
	active         int
	ready, blocked time.Time
}
type pollJob struct {
	id   int64
	host string
}
type hostLease struct{ keys map[string]bool }
type hostLeaseKey struct{}
type hostDeferred struct{ host string }

func (e *hostDeferred) Error() string {
	return fmt.Sprintf("host %s is queued or cooling down", e.host)
}

func hostGroup(u *url.URL) string {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	if host == "api.github.com" {
		host = "github.com"
	}
	return host
}
func (c Config) hostConcurrency() int {
	if c.Poll.PerHostConcurrency == 0 {
		return 1
	}
	return c.Poll.PerHostConcurrency
}
func (c Config) hostDelay() time.Duration {
	if c.Poll.PerHostDelay == "" {
		return 2 * time.Second
	}
	d, _ := time.ParseDuration(c.Poll.PerHostDelay)
	return d
}
func (p *Poller) stateLocked(host string) *hostState {
	s := p.hosts[host]
	if s == nil {
		s = &hostState{}
		p.hosts[host] = s
	}
	return s
}
func (p *Poller) reserveHost(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stateLocked(host)
	now := time.Now()
	if s.active >= p.config.hostConcurrency() || now.Before(s.ready) || now.Before(s.blocked) {
		return false
	}
	s.active++
	return true
}
func (p *Poller) releaseHost(host string) {
	p.mu.Lock()
	s := p.stateLocked(host)
	if s.active > 0 {
		s.active--
	}
	s.ready = maxTime(s.ready, time.Now().Add(p.config.hostDelay()))
	close(p.hostChanged)
	p.hostChanged = make(chan struct{})
	p.mu.Unlock()
	p.Wake()
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func (p *Poller) hostStatus(host string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stateLocked(host)
	return maxTime(s.ready, s.blocked), s.active >= p.config.hostConcurrency()
}
func (p *Poller) hostCooldown(host string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(time.Until(p.stateLocked(host).blocked), 0)
}
func (p *Poller) waitHost(ctx context.Context, host string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		s := p.stateLocked(host)
		now := time.Now()
		if now.Before(s.blocked) {
			retry := s.blocked.Sub(now)
			p.mu.Unlock()
			return &gitHTTPError{http.StatusTooManyRequests, retry}
		}
		if s.active < p.config.hostConcurrency() && !now.Before(s.ready) {
			s.active++
			p.mu.Unlock()
			return nil
		}
		changed := p.hostChanged
		delay := time.Hour
		if now.Before(s.ready) {
			delay = s.ready.Sub(now)
		}
		p.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (p *Poller) pauseHost(ctx context.Context, host string, delay time.Duration) {
	if delay <= 0 {
		return
	}
	until := time.Now().Add(min(delay, 24*time.Hour))
	p.mu.Lock()
	s := p.stateLocked(host)
	s.blocked = maxTime(s.blocked, until)
	until = s.blocked
	close(p.hostChanged)
	p.hostChanged = make(chan struct{})
	p.mu.Unlock()
	if _, err := p.store.DB.ExecContext(ctx, "INSERT INTO host_limits(host,blocked_until) VALUES(?,?) ON CONFLICT(host) DO UPDATE SET blocked_until=MAX(blocked_until,excluded.blocked_until)", host, until.UnixMilli()); err != nil && ctx.Err() == nil {
		slog.Warn("persist host cooldown", "host", host, "error", err)
	}
}
func (p *Poller) loadHostLimits() error {
	rows, err := p.store.DB.Query("SELECT host,blocked_until FROM host_limits WHERE blocked_until>?", time.Now().UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var host string
		var until int64
		if err = rows.Scan(&host, &until); err != nil {
			return err
		}
		p.mu.Lock()
		p.stateLocked(host).blocked = time.UnixMilli(until)
		p.mu.Unlock()
	}
	return rows.Err()
}
func retryAfter(header http.Header, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 86400)) * time.Second
	}
	if until, err := http.ParseTime(header.Get("Retry-After")); err == nil {
		return min(max(until.Sub(now), 0), 24*time.Hour)
	}
	return 0
}
func (p *Poller) observeHostResponse(ctx context.Context, host string, response *http.Response) time.Duration {
	now := time.Now()
	retry := retryAfter(response.Header, now)
	remaining := response.Header.Get("X-RateLimit-Remaining")
	reset := response.Header.Get("X-RateLimit-Reset")
	if remaining == "" {
		remaining = response.Header.Get("RateLimit-Remaining")
	}
	if reset == "" {
		reset = response.Header.Get("RateLimit-Reset")
	}
	exhausted := strings.TrimSpace(remaining) == "0"
	if exhausted {
		if seconds, err := strconv.ParseInt(reset, 10, 64); err == nil {
			retry = max(retry, min(max(time.Unix(seconds, 0).Sub(now), 0), 24*time.Hour))
		}
	}
	limited := response.StatusCode == http.StatusTooManyRequests || exhausted || ((response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusForbidden) && retry > 0)
	if limited {
		if retry <= 0 {
			retry = time.Minute
		}
		p.pauseHost(ctx, host, retry)
		reason := fmt.Sprintf("HTTP %d Retry-After", response.StatusCode)
		if response.StatusCode == http.StatusTooManyRequests {
			reason = "HTTP 429 rate limit"
		} else if exhausted {
			reason = "API quota exhausted"
		}
		p.mu.Lock()
		p.stateLocked(host).reason = reason
		p.mu.Unlock()
	}
	return retry
}

// Redirects reserve the target group too. They never wait while holding another
// host, avoiding cross-host redirect deadlocks; a busy target defers this sync.
func (p *Poller) reserveRedirect(req *http.Request) error {
	lease, ok := req.Context().Value(hostLeaseKey{}).(*hostLease)
	if !ok {
		return nil
	}
	host := hostGroup(req.URL)
	if lease.keys[host] {
		return nil
	}
	if !p.reserveHost(host) {
		return &hostDeferred{host}
	}
	lease.keys[host] = true
	return nil
}
