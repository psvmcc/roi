package roi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const queueRSS = `<rss version="2.0"><channel><title>Queue</title><item><guid>one</guid><title>One</title></item></channel></rss>`

func runQueue(t *testing.T, p *Poller) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("host queue did not stop")
		}
	})
	return cancel, done
}
func awaitQueue(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("queue condition timed out")
}

func TestHostQueueGroupsConfigAndMinimumDelay(t *testing.T) {
	for raw, want := range map[string]string{"https://github.com/a": "github.com", "https://api.github.com/a": "github.com", "https://GITHUB.COM.:443/a": "github.com", "https://example.org:8080": "example.org", "http://[::ffff:127.0.0.1]:80": "127.0.0.1"} {
		u, _ := url.Parse(raw)
		if got := hostGroup(u); got != want {
			t.Fatal(got, want)
		}
	}
	c := testConfig(t)
	c.Poll.PerHostDelay = ""
	if c.hostDelay() != 2*time.Second || c.hostConcurrency() != 1 {
		t.Fatal("unsafe defaults")
	}
	for _, delay := range []string{"bad", "-1s", "61m"} {
		c.Poll.PerHostDelay = delay
		if c.Validate() == nil {
			t.Fatal(delay)
		}
	}
	c.Poll.PerHostDelay = "0s"
	for _, limit := range []int{-1, 33} {
		c.Poll.PerHostConcurrency = limit
		if c.Validate() == nil {
			t.Fatal(limit)
		}
	}
	a := testApp(t)
	p := a.poller
	p.config.Poll.PerHostConcurrency = 2
	p.config.Poll.PerHostDelay = "60ms"
	first := p.reserveHost("host")
	second := p.reserveHost("host")
	third := p.reserveHost("host")
	if !first || !second || third {
		t.Fatal("concurrency cap failed")
	}
	p.releaseHost("host")
	if p.reserveHost("host") {
		t.Fatal("post-sync delay ignored")
	}
	ready, busy := p.hostStatus("host")
	if busy || !ready.After(time.Now()) {
		t.Fatal(ready, busy)
	}
	time.Sleep(70 * time.Millisecond)
	if !p.reserveHost("host") {
		t.Fatal("delay did not expire")
	}
	p.releaseHost("host")
	p.releaseHost("host")
}

func TestHostQueueRSSAndGitDoNotOverlapAndOtherHostsProgress(t *testing.T) {
	c := testConfig(t)
	c.Poll.Workers = 4
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	addFeed(t, a, "https://github.com/rss/one")
	addFeed(t, a, "https://github.com/rss/two")
	addFeed(t, a, "https://elsewhere.example/rss")
	source, _ := parseGitSource("https://github.com/team/repo", "github", "releases", false)
	storeGit(t, a, source)
	var mu sync.Mutex
	active := map[string]int{}
	maximum := map[string]int{}
	global, maxGlobal := 0, 0
	var trace []string
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		group := hostGroup(r.URL)
		mu.Lock()
		active[group]++
		global++
		maximum[group] = max(maximum[group], active[group])
		maxGlobal = max(maxGlobal, global)
		trace = append(trace, r.URL.Host+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		time.Sleep(25 * time.Millisecond)
		response := gitResponse(r, 200, queueRSS)
		if r.URL.Host == "api.github.com" {
			if r.URL.Query().Get("page") == "1" {
				rows := make([]map[string]any, 100)
				for i := range rows {
					rows[i] = map[string]any{"tag_name": fmt.Sprintf("v2-rc%d", i), "prerelease": true, "published_at": "2024-01-01T00:00:00Z"}
				}
				b, _ := json.Marshal(rows)
				response.Body = io.NopCloser(strings.NewReader(string(b)))
			} else {
				response.Body = io.NopCloser(strings.NewReader(`[{"tag_name":"v1","published_at":"2024-01-01T00:00:00Z","body":"Notes"}]`))
			}
		}
		mu.Lock()
		active[group]--
		global--
		mu.Unlock()
		return response, nil
	})
	cancel, done := runQueue(t, a.poller)
	awaitQueue(t, func() bool {
		var count int
		_ = a.store.DB.QueryRow("SELECT count(*) FROM feeds WHERE last_success>0").Scan(&count)
		return count == 4
	})
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if maximum["github.com"] != 1 || maxGlobal < 2 {
		t.Fatal("host serialization or cross-host concurrency failed", maximum, maxGlobal, trace)
	}
	// Requests to other host groups may run between Git pages. Only RSS
	// requests in the same GitHub group must be excluded from that interval.
	var githubTrace []string
	for _, line := range trace {
		if strings.HasPrefix(line, "github.com/") || strings.HasPrefix(line, "api.github.com/") {
			githubTrace = append(githubTrace, line)
		}
	}
	var pages []int
	for i, line := range githubTrace {
		if strings.HasPrefix(line, "api.github.com/") {
			pages = append(pages, i)
		}
	}
	if len(pages) != 2 || pages[1] != pages[0]+1 {
		t.Fatal("RSS interleaved with Git pagination", trace)
	}
}

func TestHostQueueBusyPrefixDoesNotStarveOtherHosts(t *testing.T) {
	a := testApp(t)
	for i := range 70 {
		addFeed(t, a, fmt.Sprintf("https://github.com/rss/%d", i))
	}
	last := addFeed(t, a, "https://other.example/rss")
	a.poller.pauseHost(t.Context(), "github.com", time.Hour)
	var calls atomic.Int32
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "github.com" {
			t.Error("blocked host requested")
		}
		calls.Add(1)
		return gitResponse(r, 200, queueRSS), nil
	})
	runQueue(t, a.poller)
	awaitQueue(t, func() bool {
		var success int64
		a.store.DB.QueryRow("SELECT last_success FROM feeds WHERE id=?", last).Scan(&success)
		return success > 0
	})
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestHostQueueCooldownPersistsAndSyncCannotBypass(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	first := addFeed(t, a, "https://github.com/rss/one")
	second := addFeed(t, a, "https://api.github.com/rss/two")
	var calls int
	transport := gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		response := gitResponse(r, 429, "")
		response.Header.Set("Retry-After", "60")
		return response, nil
	})
	a.poller.client.Transport = transport
	a.poller.fetch(t.Context(), first)
	a.poller.fetch(t.Context(), second)
	if calls != 1 || a.poller.hostCooldown("github.com") < 55*time.Second {
		t.Fatal(calls)
	}
	var failures int
	a.store.DB.QueryRow("SELECT failures FROM feeds WHERE id=?", second).Scan(&failures)
	if failures != 0 {
		t.Fatal("waiting source counted as failure")
	}
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.poller.client.Transport = transport
	cookie, csrf := adminSession(t, a)
	if w := asyncAdmin(a, cookie, "POST", "/sync", url.Values{"id": {"0"}, "csrf": {csrf}}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	a.poller.fetch(t.Context(), second)
	if calls != 1 {
		t.Fatal("restart/manual sync bypassed cooldown")
	}
	a.poller.schedule(t.Context())
	if len(a.poller.jobs) != 0 {
		t.Fatal("blocked jobs occupied workers")
	}
}

func TestHostQueueRateHeadersAndNonRateErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers http.Header
		blocked bool
	}{
		{"429 default", 429, http.Header{}, true},
		{"403 permission", 403, http.Header{"X-Ratelimit-Remaining": {"5"}, "X-Ratelimit-Reset": {"1999999999"}}, false},
		{"403 secondary", 403, http.Header{"Retry-After": {"5"}}, true},
		{"503 retry date", 503, http.Header{"Retry-After": {time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)}}, true},
		{"503 no retry", 503, http.Header{}, false},
		{"success quota exhausted", 200, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(time.Now().Add(time.Minute).Unix())}}, true},
		{"gitlab quota", 200, http.Header{"Ratelimit-Remaining": {"0"}, "Ratelimit-Reset": {fmt.Sprint(time.Now().Add(time.Minute).Unix())}}, true},
		{"unknown reset", 200, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"bad"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			response := &http.Response{StatusCode: tc.status, Header: tc.headers}
			a.poller.observeHostResponse(t.Context(), "host", response)
			if (a.poller.hostCooldown("host") > 0) != tc.blocked {
				t.Fatal(tc.name, a.poller.hostCooldown("host"))
			}
		})
	}
	if got := retryAfter(http.Header{"Retry-After": {"9999999999999"}}, time.Now()); got != 24*time.Hour {
		t.Fatal(got)
	}
}

func TestHostQueueRedirectReservationAndCancelledSyncRelease(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://one.example/rss")
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "one.example" {
			resp := gitResponse(r, 302, "")
			resp.Header.Set("Location", "https://two.example/rss")
			return resp, nil
		}
		return gitResponse(r, 200, queueRSS), nil
	})
	if !a.poller.reserveHost("two.example") {
		t.Fatal("initial reservation failed")
	}
	start := time.Now()
	a.poller.fetch(t.Context(), id)
	if time.Since(start) > time.Second {
		t.Fatal("redirect waited while holding first host")
	}
	var failures int
	a.store.DB.QueryRow("SELECT failures FROM feeds WHERE id=?", id).Scan(&failures)
	if failures != 0 {
		t.Fatal("deferred redirect counted as failure")
	}
	a.poller.releaseHost("two.example")
	a.poller.fetch(t.Context(), id)
	var success int64
	a.store.DB.QueryRow("SELECT last_success FROM feeds WHERE id=?", id).Scan(&success)
	if success == 0 {
		t.Fatal("redirect did not resume")
	}
	for _, host := range []string{"one.example", "two.example"} {
		_, busy := a.poller.hostStatus(host)
		if busy {
			t.Fatal("redirect lease leaked", host)
		}
	}
	started := make(chan struct{})
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	done := make(chan struct{})
	go func() { defer close(done); a.poller.fetch(context.Background(), id) }()
	<-started
	a.poller.Cancel(id)
	<-done
	if !a.poller.reserveHost("one.example") {
		t.Fatal("cancelled sync retained host lock")
	}
	a.poller.releaseHost("one.example")
}

func TestHostQueueWaitersDiscoveryAndShutdown(t *testing.T) {
	a := testApp(t)
	p := a.poller
	p.config.Poll.PerHostDelay = "20ms"
	if !p.reserveHost("host") {
		t.Fatal("reserve failed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	acquired := make(chan error, 1)
	go func() { acquired <- p.waitHost(ctx, "host") }()
	p.releaseHost("host")
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	p.releaseHost("host")
	p.pauseHost(t.Context(), "host", time.Minute)
	if err := p.waitHost(t.Context(), "host"); err == nil {
		t.Fatal("rate-limited waiter succeeded")
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if err := p.waitHost(stopped, "other"); err == nil {
		t.Fatal("cancelled waiter succeeded")
	}
	if !p.reserveHost("waiting") {
		t.Fatal("reserve failed")
	}
	short, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	if err := p.waitHost(short, "waiting"); err == nil {
		t.Fatal("busy waiter ignored cancellation")
	}
	p.releaseHost("waiting")
	// A queued source removed before a worker starts must release its reservation.
	id := addFeed(t, a, "https://old.example/rss")
	if !p.reserveHost("old.example") {
		t.Fatal("reserve failed")
	}
	mustExec(t, a, "UPDATE feeds SET active=0 WHERE id=?", id)
	p.fetchReserved(t.Context(), id, "old.example")
	_, busy := p.hostStatus("old.example")
	if busy {
		t.Fatal("removed source leaked reservation")
	}
	id = addFeed(t, a, "https://new.example/rss")
	p.config.Poll.PerHostDelay = "0s"
	if err := p.waitHost(t.Context(), "old.example"); err != nil {
		t.Fatal(err)
	}
	p.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) { return gitResponse(r, 200, queueRSS), nil })
	p.fetchReserved(t.Context(), id, "old.example")
	p.mu.Lock()
	for host, s := range p.hosts {
		if s.active != 0 {
			t.Errorf("leaked host %s", host)
		}
	}
	p.mu.Unlock()
	a.Close()
	if err := p.loadHostLimits(); err == nil {
		t.Fatal("closed cooldown store accepted")
	}
	p.pauseHost(context.Background(), "new", time.Minute)
}

func TestHostQueueGitStopsAtExhaustedBudget(t *testing.T) {
	a := testApp(t)
	source, _ := parseGitSource("https://github.com/team/repo", "github", "releases", false)
	id := storeGit(t, a, source)
	var calls int
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		rows := make([]map[string]any, 100)
		for i := range rows {
			rows[i] = map[string]any{"tag_name": fmt.Sprintf("v2-rc%d", i), "prerelease": true, "published_at": "2024-01-01T00:00:00Z"}
		}
		b, _ := json.Marshal(rows)
		resp := gitResponse(r, 200, string(b))
		resp.Header.Set("X-RateLimit-Remaining", "0")
		resp.Header.Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
		return resp, nil
	})
	a.poller.fetch(t.Context(), id)
	if calls != 1 {
		t.Fatal("pagination continued after quota exhaustion", calls)
	}
	var failures int
	a.store.DB.QueryRow("SELECT failures FROM feeds WHERE id=?", id).Scan(&failures)
	if failures != 1 {
		t.Fatal("partial sync was not deferred", failures)
	}
}

func TestHostQueueShutdownDrainsReservationsAndQueueCapacity(t *testing.T) {
	a := testApp(t)
	p := a.poller
	for i := range 5 {
		addFeed(t, a, fmt.Sprintf("https://host%d.example/rss", i))
	}
	p.schedule(t.Context())
	if len(p.jobs) != cap(p.jobs) {
		t.Fatal("queue not filled")
	}
	p.mu.Lock()
	reserved := 0
	for _, s := range p.hosts {
		reserved += s.active
	}
	p.mu.Unlock()
	if reserved != cap(p.jobs) {
		t.Fatal("unsent queue item retained a reservation", reserved)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Run(ctx)
	if len(p.jobs) != 0 {
		t.Fatal("shutdown did not drain queue")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for host, s := range p.hosts {
		if s.active != 0 {
			t.Fatal("shutdown leaked reservation", host, s.active)
		}
	}
}
