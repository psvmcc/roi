package roi

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Keep the first denial per source/status in each 30-second window, and cap
// denial writes globally. Successful logins, approvals and revocations are
// always retained. Suppressed denials remain observable through a counter.
type auditGate struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	bucket tokenBucket
}

func (g *auditGate) allow(event APIRequest, now time.Time) bool {
	if event.Status != 401 && event.Status != 403 && event.Status != 429 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = map[string]time.Time{}
	}
	for key, at := range g.seen {
		if now.Sub(at) >= 30*time.Second {
			delete(g.seen, key)
		}
	}
	// Authentication failures must not disappear behind earlier forbidden API
	// mutations from the same client. Keep a bounded set of event categories.
	kind := "request"
	if event.Path == "/login" || strings.HasSuffix(event.Path, "/accounts/ClientLogin") {
		kind = "login"
	}
	key := event.Realm + "|" + event.IP + "|" + strconv.Itoa(event.Status) + "|" + kind
	if _, ok := g.seen[key]; ok {
		return false
	}
	if len(g.seen) >= 4096 {
		return false
	}
	limiter := requestLimiter{rate: 2, burst: 20}
	if !limiter.take(&g.bucket, now) {
		return false
	}
	g.seen[key] = now
	return true
}
