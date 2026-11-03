package roi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/mmcdole/gofeed"
)

type Poller struct {
	store             *Store
	config            Config
	client            *http.Client
	mu                sync.Mutex
	checkpointPending bool
	running           map[int64]bool
	cancels           map[int64]context.CancelFunc
	hosts             map[string]*hostState
	hostChanged       chan struct{}
	jobs              chan pollJob
	wake              chan struct{}
	metrics           *metrics.Set
	resources         *parseGate
}

func NewPoller(s *Store, c Config, m *metrics.Set) *Poller {
	d := &net.Dialer{Timeout: c.timeout()}
	transport := &http.Transport{MaxIdleConns: 32, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: c.timeout(), MaxResponseHeaderBytes: 64 << 10}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if e != nil {
			return nil, e
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for host")
		}
		for _, ip := range ips {
			if !c.outboundIPAllowed(ip) {
				return nil, fmt.Errorf("private or reserved address blocked")
			}
		}
		var last error
		for _, ip := range ips {
			conn, e := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		return nil, last
	}
	p := &Poller{store: s, config: c, running: map[int64]bool{}, cancels: map[int64]context.CancelFunc{}, hosts: map[string]*hostState{}, hostChanged: make(chan struct{}), jobs: make(chan pollJob, c.Poll.Workers), wake: make(chan struct{}, 1), metrics: m}
	client := &http.Client{Transport: transport, Timeout: c.timeout(), CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if err := validFeedURL(req.URL.String()); err != nil {
			return err
		}
		return p.reserveRedirect(req)
	}}
	p.client = client
	p.resources = newParseGate(c.security())
	return p
}
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && netip.MustParsePrefix("64:ff9b::/96").Contains(ip) {
		b := ip.As16()
		return publicIP(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if ip.Is6() && netip.MustParsePrefix("2002::/16").Contains(ip) {
		b := ip.As16()
		return publicIP(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, s := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b:1::/48", "2001::/32", "::/96"} {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}
func (p *Poller) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range p.config.Poll.Workers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-p.jobs:
					p.fetchReserved(ctx, job.id, job.host)
					p.mu.Lock()
					delete(p.running, job.id)
					p.checkpointPending = true
					p.mu.Unlock()
					p.Wake()
				}
			}
		})
	}
	defer func() {
		wg.Wait()
		for {
			select {
			case job := <-p.jobs:
				p.releaseHost(job.host)
				p.mu.Lock()
				delete(p.running, job.id)
				p.mu.Unlock()
			default:
				p.client.CloseIdleConnections()
				return
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	cleanup := time.NewTicker(time.Minute)
	defer cleanup.Stop()
	p.schedule(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.schedule(ctx)
		case <-p.wake:
			p.schedule(ctx)
		case <-cleanup.C:
			p.maintain(ctx)
		}
		p.mu.Lock()
		checkpoint := len(p.running) == 0 && p.checkpointPending
		if checkpoint {
			p.checkpointPending = false
		}
		p.mu.Unlock()
		if checkpoint {
			p.checkpoint(ctx, true)
		}
	}
}
func (p *Poller) checkpoint(ctx context.Context, idle bool) {
	if _, err := p.store.Checkpoint(ctx, idle); err != nil && ctx.Err() == nil {
		slog.Warn("WAL checkpoint", "error", err)
	}
}
func (p *Poller) schedule(ctx context.Context) {
	r, e := p.store.DB.QueryContext(ctx, "SELECT id,url FROM feeds WHERE active=1 AND paused=0 AND next_fetch<=? ORDER BY next_fetch,id", time.Now().Unix())
	if e != nil {
		if ctx.Err() == nil {
			slog.Error("schedule", "error", e)
		}
		return
	}
	ids := []int64{}
	hosts := map[int64]string{}
	for r.Next() {
		var id int64
		var raw string
		if e = r.Scan(&id, &raw); e != nil {
			break
		}
		ids = append(ids, id)
		if u, err := url.Parse(raw); err == nil {
			hosts[id] = hostGroup(u)
		}
	}
	if e == nil {
		e = r.Err()
	}
	r.Close()
	if e != nil {
		slog.Error("schedule scan", "error", e)
		return
	}
	for _, id := range ids {
		p.mu.Lock()
		running := p.running[id]
		p.mu.Unlock()
		if running || ctx.Err() != nil || !p.reserveHost(hosts[id]) {
			continue
		}
		p.mu.Lock()
		select {
		case p.jobs <- pollJob{id, hosts[id]}:
			p.running[id] = true
			p.mu.Unlock()
		default:
			p.stateLocked(hosts[id]).active--
			close(p.hostChanged)
			p.hostChanged = make(chan struct{})
			p.mu.Unlock()
			return
		}
	}
}
func (p *Poller) fetch(ctx context.Context, id int64) { p.fetchReserved(ctx, id, "") }
func (p *Poller) fetchReserved(ctx context.Context, id int64, reserved string) {
	lease := &hostLease{keys: map[string]bool{}}
	if reserved != "" {
		lease.keys[reserved] = true
	}
	defer func() {
		for host := range lease.keys {
			p.releaseHost(host)
		}
	}()
	ctx = context.WithValue(ctx, hostLeaseKey{}, lease)
	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancels[id] = cancel
	p.mu.Unlock()
	defer func() { cancel(); p.mu.Lock(); delete(p.cancels, id); p.mu.Unlock() }()
	var raw, title, etag, modified, userAgent string
	var failures int
	var git GitSource
	e := p.store.DB.QueryRowContext(ctx, "SELECT url,title,etag,modified,failures,git_provider,git_repo,git_mode,git_preview,git_revision,icon_url,user_agent,git_tag_filter FROM feeds WHERE id=? AND active=1 AND paused=0", id).Scan(&raw, &title, &etag, &modified, &failures, &git.Provider, &git.Repo, &git.Mode, &git.Preview, &git.Revision, &git.Icon, &userAgent, &git.TagFilter)
	if e != nil {
		return
	}
	req, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		slog.Error("feed request", "feed_id", id, "feed_title", title, "feed_url", raw, "error", e)
		return
	}
	host := hostGroup(req.URL)
	if reserved != "" && reserved != host {
		delete(lease.keys, reserved)
		p.releaseHost(reserved)
	}
	if !lease.keys[host] {
		if !p.reserveHost(host) {
			return
		}
		lease.keys[host] = true
	}
	started := time.Now()
	if err := p.resources.acquire(ctx, 2*p.config.Poll.MaxBytes); err != nil {
		return
	}
	defer p.resources.release(2 * p.config.Poll.MaxBytes)
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if modified != "" {
		req.Header.Set("If-Modified-Since", modified)
	}
	var resp *http.Response
	status, added := 0, 0
	var parsed *gofeed.Feed
	var retry time.Duration
	stage := "network"
	if git.Provider != "" {
		git.URL = raw
		stage = "parse"
		parsed, e = p.fetchGit(ctx, git, id)
		status = 200
		var ge *gitHTTPError
		if errors.As(e, &ge) {
			status = ge.Status
			retry = ge.Retry
		} else if e != nil {
			status = 0
		}
	} else {
		resp, e = p.client.Do(req)
		if e == nil {
			defer resp.Body.Close()
			status = resp.StatusCode
			responseHost := host
			if resp.Request != nil {
				responseHost = hostGroup(resp.Request.URL)
			}
			retry = p.observeHostResponse(ctx, responseHost, resp)
			if responseHost != host && retry > 0 {
				p.pauseHost(ctx, host, retry)
			}
			switch {
			case status == 304:
			case status >= 200 && status < 300:
				var b []byte
				b, e = io.ReadAll(io.LimitReader(resp.Body, p.config.Poll.MaxBytes+1))
				if e == nil && int64(len(b)) > p.config.Poll.MaxBytes {
					e = fmt.Errorf("feed exceeds max_bytes")
				}
				if e == nil {
					stage = "parse"
					e = checkFeedDocument(b)
					if e == nil {
						parsed, e = gofeed.NewParser().ParseString(string(b))
					}
					if e == nil && len(parsed.Items) > 10000 {
						e = fmt.Errorf("feed contains more than 10000 articles")
					}
				}
			default:
				e = fmt.Errorf("HTTP %d", status)
			}

		}
	}
	var deferred *hostDeferred
	if errors.As(e, &deferred) {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if e == nil && parsed != nil {
		stage = "storage"
		added, e = p.saveEntriesRevision(ctx, id, parsed, time.Now(), git.Revision)
	}
	delay := p.config.interval()
	message := ""
	if e != nil {
		category := syncErrorCategory(e, status, stage)
		message = category + ": " + e.Error()
		p.metrics.GetOrCreateCounter(fmt.Sprintf("roi_fetch_failures_total{category=%q}", strings.ToLower(category))).Inc()
		failures++
		delay = min(delay*time.Duration(1<<min(failures, 10)), 24*time.Hour)
		p.metrics.GetOrCreateCounter("roi_fetch_errors_total").Inc()
	} else {
		failures = 0
	}
	delay = time.Duration(float64(delay) * (0.95 + rand.Float64()*0.1))
	delay = max(delay, retry)
	now := time.Now().Unix()
	// Rounding down could schedule another fetch in this very second when the
	// configured interval is 1s and jitter reduces it slightly.
	next := (time.Now().Add(delay).UnixNano() + int64(time.Second) - 1) / int64(time.Second)
	tx, err := p.store.DB.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("fetch state", "feed_id", id, "feed_title", title, "feed_url", raw, "error", err)
		return
	}
	defer tx.Rollback()
	var active bool
	var revision int64
	if err = tx.QueryRowContext(ctx, "SELECT active,git_revision FROM feeds WHERE id=?", id).Scan(&active, &revision); err != nil || !active || revision != git.Revision {
		return
	}
	if e == nil {
		newETag, newModified := etag, modified
		if status != 304 && resp != nil {
			newETag = resp.Header.Get("ETag")
			newModified = resp.Header.Get("Last-Modified")
		}
		_, err = tx.ExecContext(ctx, "UPDATE feeds SET last_attempt=?,last_success=?,next_fetch=?,error='',failures=0,etag=?,modified=? WHERE id=?", started.Unix(), now, next, newETag, newModified, id)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE feeds SET last_attempt=?,next_fetch=?,error=?,failures=? WHERE id=?", started.Unix(), next, message, failures, id)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO fetches(feed_id,started,finished,status,added,error) VALUES(?,?,?,?,?,?)", id, started.Unix(), now, status, added, message)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		slog.Error("fetch state", "feed_id", id, "feed_title", title, "feed_url", raw, "error", err)
	}
	p.metrics.GetOrCreateCounter("roi_fetch_total").Inc()
	p.metrics.GetOrCreateHistogram("roi_fetch_duration_seconds").Update(time.Since(started).Seconds())
	p.metrics.GetOrCreateCounter("roi_entries_received_total").Add(added)
	if e != nil {
		slog.Warn("feed update failed", "feed_id", id, "feed_title", title, "feed_url", raw, "error", message)
	}
}
func (p *Poller) saveEntries(ctx context.Context, id int64, f *gofeed.Feed, at time.Time) (int, error) {
	return p.saveEntriesRevision(ctx, id, f, at, -1)
}
func (p *Poller) saveEntriesRevision(ctx context.Context, id int64, f *gofeed.Feed, at time.Time, expected int64) (int, error) {
	now := at.Unix()
	received := at.UnixMicro()
	tx, e := p.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	var active bool
	var revision int64
	if e = tx.QueryRowContext(ctx, "SELECT active,git_revision FROM feeds WHERE id=?", id).Scan(&active, &revision); e == sql.ErrNoRows {
		return 0, nil
	} else if e != nil {
		return 0, e
	}
	if !active || (expected >= 0 && revision != expected) {
		return 0, nil
	}
	added := 0
	for i := len(f.Items) - 1; i >= 0; i-- {
		item := f.Items[i]
		guid := item.GUID
		if guid == "" {
			guid = item.Link
		}
		if guid == "" {
			h := sha256.Sum256([]byte(item.Title + "\x00" + item.Published))
			guid = fmt.Sprintf("sha256:%x", h)
		}
		preview := item.Custom["roi_preview"] == "1"
		content := item.Content
		if content == "" {
			content = item.Description
		}
		author := ""
		if item.Author != nil {
			author = item.Author.Name
		}
		published := now
		sourceDated := false
		if item.PublishedParsed != nil && !item.PublishedParsed.IsZero() {
			sourceDated = true
			published = item.PublishedParsed.Unix()
		} else if item.UpdatedParsed != nil && !item.UpdatedParsed.IsZero() {
			sourceDated = true
			published = item.UpdatedParsed.Unix()
		}
		var oldID, oldPublished int64
		e = tx.QueryRowContext(ctx, "SELECT id,published FROM entries WHERE feed_id=? AND guid=?", id, guid).Scan(&oldID, &oldPublished)
		if e != nil && e != sql.ErrNoRows {
			return 0, e
		}
		if e == sql.ErrNoRows {
			res, err := tx.ExecContext(ctx, `INSERT INTO entries(feed_id,guid,title,url,content,author,published,received,updated,git_preview) SELECT ?,?,?,?,?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM tombstones WHERE feed_id=? AND guid IN (?,?))`, id, guid, item.Title, item.Link, content, author, published, received, now, preview, id, guid, tombstoneDigest(guid))
			if err != nil {
				return 0, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			added += int(n)
		} else {
			if !sourceDated {
				published = oldPublished
			}
			_, e = tx.ExecContext(ctx, `UPDATE entries SET title=?,url=?,content=?,author=?,published=?,updated=?,git_preview=? WHERE id=? AND (title<>? OR url<>? OR content<>? OR author<>? OR published<>? OR git_preview<>?)`, item.Title, item.Link, content, author, published, now, preview, oldID, item.Title, item.Link, content, author, published, preview)
			if e != nil {
				return 0, e
			}
		}
		if item.Custom["roi_git_object"] != "" {
			if _, e = tx.ExecContext(ctx, "UPDATE entries SET git_object=?,render_version=? WHERE feed_id=? AND guid=? AND (git_object<>? OR render_version<>?)", item.Custom["roi_git_object"], gitRenderVersion, id, guid, item.Custom["roi_git_object"], gitRenderVersion); e != nil {
				return 0, e
			}
		}
	}
	_, e = tx.ExecContext(ctx, "UPDATE feeds SET title=CASE WHEN title=url AND ?<>'' THEN ? ELSE title END,site=CASE WHEN ?='' THEN site ELSE ? END,last_published=COALESCE((SELECT MAX(published) FROM entries WHERE feed_id=?),0),last_new=CASE WHEN ?>0 THEN ? ELSE last_new END WHERE id=?", f.Title, f.Title, strings.TrimSpace(f.Link), f.Link, id, added, now, id)
	if e != nil {
		return 0, e
	}
	return added, tx.Commit()
}

func (p *Poller) Cancel(id int64) {
	p.mu.Lock()
	cancel := p.cancels[id]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *Poller) maintain(ctx context.Context) {
	if _, err := p.store.DB.ExecContext(ctx, "DELETE FROM git_http_cache WHERE checked<?", time.Now().AddDate(0, 0, -7).Unix()); err != nil && ctx.Err() == nil {
		slog.Warn("trim Git HTTP cache", "error", err)
	}
	if err := p.store.CompactTombstones(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("compact tombstone keys", "error", err)
	}
	if _, err := p.store.DB.ExecContext(ctx, "DELETE FROM host_limits WHERE blocked_until<=?", time.Now().UnixMilli()); err != nil && ctx.Err() == nil {
		slog.Warn("trim host cooldowns", "error", err)
	}
	p.mu.Lock()
	now := time.Now()
	for host, s := range p.hosts {
		if s.active == 0 && !now.Before(s.ready) && !now.Before(s.blocked) {
			delete(p.hosts, host)
		}
	}
	p.mu.Unlock()
	if n, e := p.store.Rotate(p.config, time.Now()); e != nil {
		slog.Error("rotation", "error", e)
	} else if n > 0 {
		p.metrics.GetOrCreateCounter("roi_rotation_deleted_total").Add(int(n))
	}
	if _, e := p.store.DB.ExecContext(ctx, "DELETE FROM fetches WHERE finished < ?", time.Now().AddDate(0, 0, -30).Unix()); e != nil {
		slog.Error("trim fetch log", "error", e)
	}
	if _, err := p.store.DB.ExecContext(ctx, "DELETE FROM auth_events WHERE created<?", time.Now().AddDate(0, 0, -30).Unix()); err != nil && ctx.Err() == nil {
		slog.Warn("trim audit history", "error", err)
	}
	if _, err := p.store.DB.ExecContext(ctx, "DELETE FROM auth_sessions WHERE (expires>0 AND expires<?) OR (revoked=1 AND last_seen<?)", time.Now().AddDate(0, 0, -30).Unix(), time.Now().AddDate(0, 0, -30).Unix()); err != nil && ctx.Err() == nil {
		slog.Warn("trim session history", "error", err)
	}
	p.mu.Lock()
	idle := len(p.running) == 0
	p.mu.Unlock()
	p.checkpoint(ctx, idle)
}
