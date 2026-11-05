package roi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

type App struct {
	instanceLock *os.File
	closeOnce    sync.Once
	closeError   error
	config       Config
	store        *Store
	poller       *Poller
	metrics      *metrics.Set
	mu           sync.Mutex
	keyRotation  sync.Mutex
	signingKey   string
	legacyKey    string
	limiter      *requestLimiter
	apiSlots     chan struct{}
	auditGate    auditGate
}

func New(c Config) (*App, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if e := checkStoragePermissions(c); e != nil {
		return nil, e
	}
	lock, e := lockInstance(c.Database)
	if e != nil {
		return nil, e
	}
	success := false
	defer func() {
		if !success {
			_ = lock.Close()
		}
	}()
	s, e := OpenStore(c.Database)
	if e != nil {
		return nil, e
	}
	key, e := s.SigningKey()
	if e != nil {
		s.DB.Close()
		return nil, e
	}
	if e = s.initializeSessions(c); e != nil {
		s.DB.Close()
		return nil, e
	}
	m := metrics.NewSet()
	a := &App{config: c, store: s, metrics: m, instanceLock: lock}
	for _, name := range []string{"roi_fetch_total", "roi_fetch_errors_total", "roi_entries_received_total", "roi_rotation_deleted_total", "roi_backups_total", "roi_backup_errors_total"} {
		m.GetOrCreateCounter(name)
	}
	a.signingKey = key
	if e = s.DB.QueryRow("SELECT COALESCE((SELECT value FROM auth_secrets WHERE name='api_legacy_key'),'')").Scan(&a.legacyKey); e != nil {
		s.DB.Close()
		return nil, e
	}
	a.limiter = newRequestLimiter(c.security())
	a.apiSlots = make(chan struct{}, c.security().APIConcurrency)
	a.poller = NewPoller(s, c, m)
	if err := a.poller.loadHostLimits(); err != nil {
		s.DB.Close()
		return nil, err
	}
	success = true
	return a, nil
}
func (a *App) Close() error {
	a.closeOnce.Do(func() { a.closeError = errors.Join(a.store.DB.Close(), a.instanceLock.Close()) })
	return a.closeError
}
func (a *App) allowLogin(r *http.Request) bool {
	realm := "admin"
	if strings.HasSuffix(r.URL.Path, "/accounts/ClientLogin") {
		realm = "api"
	}
	limits := a.config.security()
	ok, e := a.store.TakeLogin(realm, a.clientIP(r), limits.LoginPerMinute, limits.AccountLoginPerMinute, time.Now())
	if e != nil {
		slog.Error("login limiter unavailable", "error", e)
		return false
	}
	return ok
}
func (a *App) RotateAPIKey() error {
	a.keyRotation.Lock()
	defer a.keyRotation.Unlock()
	key, e := a.store.RotateSigningKey()
	if e != nil {
		return e
	}
	a.mu.Lock()
	a.signingKey = key
	a.legacyKey = ""
	a.mu.Unlock()
	return nil
}
func (a *App) requestError(w http.ResponseWriter, r *http.Request, e error) {
	var se *statusError
	if errors.As(e, &se) {
		http.Error(w, se.Message, se.Status)
		return
	}
	var ce clientError
	if errors.As(e, &ce) {
		http.Error(w, string(ce), http.StatusBadRequest)
		return
	}
	slog.Error("request failed", "path", r.URL.Path, "error", e)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
func (a *App) wrap(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.metrics.GetOrCreateCounter(fmt.Sprintf("roi_http_requests_total{service=%q}", service)).Inc()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		defer func() {
			if v := recover(); v != nil {
				slog.Error("handler panic", "error", v)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (a *App) APIHandler() http.Handler {
	return a.logAPI(a.wrap("api", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, a.config.API.Prefix+"/") {
			http.NotFound(w, r)
			return
		}
		if !a.limiter.allow(a.clientIP(r), time.Now()) {
			rateLimited(w)
			return
		}
		if !a.limiter.allowAccount(time.Now()) {
			rateLimited(w)
			return
		}
		a.api(w, r)
	})))
}
func (a *App) OpsHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if e := a.store.DB.PingContext(ctx); e != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	m.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var feeds, entries, unread uint64
		if e := a.store.DB.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM feeds WHERE active=1),COALESCE(SUM(count),0),COALESCE(SUM(unread),0) FROM feed_stats").Scan(&feeds, &entries, &unread); e != nil {
			a.requestError(w, r, e)
			return
		}
		var visible uint64
		if e := a.store.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(s.visible_unread),0) FROM feed_stats s JOIN feeds f ON f.id=s.feed_id WHERE f.active=1").Scan(&visible); e != nil {
			a.requestError(w, r, e)
			return
		}
		usage, e := a.store.Space(ctx)
		if e != nil {
			a.requestError(w, r, e)
			return
		}
		metrics.WriteGaugeUint64(w, "roi_feeds", feeds)
		metrics.WriteGaugeUint64(w, "roi_entries", entries)
		metrics.WriteGaugeUint64(w, "roi_unread_entries", unread)
		metrics.WriteGaugeUint64(w, "roi_reader_unread_entries", visible)
		metrics.WriteGaugeUint64(w, "roi_database_free_bytes", uint64(usage.FreeBytes))
		metrics.WriteGaugeUint64(w, "roi_tombstones", uint64(usage.Tombstones))
		metrics.WriteGaugeUint64(w, "roi_tombstone_key_bytes", uint64(usage.TombstoneBytes))
		for _, suffix := range []string{"", "-wal"} {
			if st, e := os.Stat(a.config.Database + suffix); e == nil {
				name := "roi_database_bytes"
				if suffix != "" {
					name = "roi_wal_bytes"
				}
				metrics.WriteGaugeUint64(w, name, uint64(st.Size()))
			}
		}
		a.metrics.WritePrometheus(w)
		metrics.WriteProcessMetrics(w)
	})
	return a.wrap("ops", m)
}
func (a *App) Run(ctx context.Context) error {
	handlers := []http.Handler{a.APIHandler(), a.AdminHandler(), a.OpsHandler()}
	addresses := []string{a.config.API.Listen, a.config.Admin.Listen, a.config.Ops.Listen}
	listeners := []net.Listener{}
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for _, address := range addresses {
		l, e := net.Listen("tcp", address)
		if e != nil {
			return e
		}
		listeners = append(listeners, l)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { a.poller.Run(runCtx) })
	wg.Go(func() { a.runBackups(runCtx) })
	servers := []*http.Server{}
	errs := make(chan error, 3)
	for i, h := range handlers {
		s := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
		servers = append(servers, s)
		l := listeners[i]
		wg.Go(func() {
			e := s.Serve(l)
			if e != nil && !errors.Is(e, http.ErrServerClosed) {
				errs <- e
			}
		})
		slog.Info("listening", "address", addresses[i])
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-errs:
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	for _, s := range servers {
		if e := s.Shutdown(shutdown); e != nil {
			_ = s.Close()
			if result == nil {
				result = e
			}
		}
	}
	wg.Wait()
	return result
}
