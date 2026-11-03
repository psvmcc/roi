package roi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/crypto/bcrypt"
)

func TestIndependentSigningKeyMigrationPersistenceAndRevocation(t *testing.T) {
	c := testConfig(t)
	a, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "preserved"})
	token := approvedToken(t, a)
	// The former hash-only construction and a separate instance with the same
	// credentials must not be able to forge this database's bearer tokens.
	legacy := fmt.Sprintf("%d.%s", time.Now().Add(time.Hour).Unix(), randomToken())
	legacy += "." + sign(c.APIAuth.Username+":"+c.APIAuth.PasswordHash, legacy)
	if a.validToken(legacy) {
		t.Fatal("legacy hash-only token accepted")
	}
	forged := &App{config: c}
	if a.validToken(forged.issueToken()) {
		t.Fatal("configuration hash alone can forge tokens")
	}
	key, e := a.store.SigningKey()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	a, e = New(c)
	if e != nil {
		t.Fatal(e)
	}
	if !a.validToken(token) {
		t.Fatal("restart invalidated token")
	}
	key2, e := a.store.SigningKey()
	if e != nil || key != key2 {
		t.Fatal("key changed on restart", e)
	}
	if e = a.RotateAPIKey(); e != nil {
		t.Fatal(e)
	}
	if a.validToken(token) {
		t.Fatal("revoked token accepted")
	}
	token = approvedToken(t, a)
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	// A v1 database migrates without losing articles. Simulate its exact absence
	// of the new auth tables rather than merely changing the version number.
	s, e := OpenStore(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	rollbackReviewMigration(t, s.DB)
	if _, e = s.DB.Exec("DROP TABLE auth_secrets; DROP TABLE login_limits; DROP TABLE auth_sessions; DROP TABLE auth_events; DROP TRIGGER git_folder_only; ALTER TABLE feeds DROP COLUMN user_agent; ALTER TABLE feeds DROP COLUMN git_provider; ALTER TABLE feeds DROP COLUMN git_repo; ALTER TABLE feeds DROP COLUMN git_mode; ALTER TABLE feeds DROP COLUMN git_preview; ALTER TABLE feeds DROP COLUMN git_revision; ALTER TABLE feeds DROP COLUMN icon_url; ALTER TABLE entries DROP COLUMN git_preview; ALTER TABLE feeds DROP COLUMN git_tag_filter; PRAGMA user_version=1"); e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	a, e = New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	fs, e := a.store.Feeds()
	if e != nil || len(fs) != 1 || fs[0].Count != 1 {
		t.Fatal(fs, e)
	}
	var version int
	if e = a.store.DB.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 12 {
		t.Fatal(version, e)
	}
	if a.validToken(token) {
		t.Fatal("migration retained old signing key")
	}
	for _, v := range []string{"", "v2.invalid." + randomToken() + "." + randomToken(), "v2.1." + randomToken() + "." + randomToken()} {
		if a.validToken(v) {
			t.Fatal("invalid/expired token accepted")
		}
	}
}
func TestCredentialChangeAndCorruptSigningKey(t *testing.T) {
	c := testConfig(t)
	a, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	token := approvedToken(t, a)
	a.Close()
	hash, e := bcrypt.GenerateFromPassword([]byte("changed-password-123"), bcrypt.MinCost)
	if e != nil {
		t.Fatal(e)
	}
	c.APIAuth.PasswordHash = string(hash)
	a, e = New(c)
	if e != nil {
		t.Fatal(e)
	}
	if a.validToken(token) {
		t.Fatal("hash change did not revoke tokens")
	}
	mustExec(t, a, "UPDATE auth_secrets SET value='corrupt' WHERE name='api'")
	if _, e = a.store.SigningKey(); e == nil {
		t.Fatal("invalid persisted key accepted")
	}
	a.Close()
	if broken, e := New(c); e == nil {
		broken.Close()
		t.Fatal("app started with corrupt signing key")
	}
}
func TestAdminRevokeTokensAndConcurrentRotation(t *testing.T) {
	a := testApp(t)
	c, csrf := adminSession(t, a)
	token := approvedToken(t, a)
	if w := adminRequest(a, c, "POST", "/revoke-api-tokens", nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(a, c, "POST", "/revoke-api-tokens", url.Values{"csrf": {csrf}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if a.validToken(token) {
		t.Fatal("admin revocation failed")
	}
	if w := adminRequest(a, c, "GET", "/", nil); w.Code != 200 {
		t.Fatal("revocation killed admin session")
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if e := a.RotateAPIKey(); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	stored, e := a.store.SigningKey()
	if e != nil || stored != a.signingKey {
		t.Fatal("rotation order diverged from database", e)
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	if e = a.RotateAPIKey(); e == nil {
		t.Fatal("ignored rotation storage error")
	}
}

func TestTrustedProxyAddressSelection(t *testing.T) {
	a := testApp(t)
	a.config.Security.TrustedProxies = []string{"127.0.0.1/32", "10.0.0.0/8", "::1/128"}
	for _, tc := range []struct{ peer, xff, want string }{{"192.0.2.1:123", "203.0.113.9", "192.0.2.1"}, {"127.0.0.1:123", "203.0.113.9", "203.0.113.9"}, {"127.0.0.1:123", "198.51.100.7, 203.0.113.9", "203.0.113.9"}, {"127.0.0.1:123", "203.0.113.9, 10.0.0.2", "203.0.113.9"}, {"127.0.0.1:123", "invalid", "127.0.0.1"}, {"127.0.0.1:123", "", "127.0.0.1"}, {"[::1]:123", "2001:db8::5", "2001:db8::5"}, {"invalid", "203.0.113.9", "unknown"}, {"invalid:123", "", "unknown"}} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.xff)
		if got := a.clientIP(r); got != tc.want {
			t.Fatalf("%+v: %s", tc, got)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:123"
	r.Header.Set("X-Forwarded-For", strings.Repeat("1.1.1.1,", 21))
	if a.clientIP(r) != "127.0.0.1" {
		t.Fatal("overlong proxy chain accepted")
	}
}
func TestPersistedLoginBudgets(t *testing.T) {
	c := testConfig(t)
	a, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Unix(12000, 0)
	for i := 0; i < 2; i++ {
		ok, e := a.store.TakeLogin("api", "client", 2, 3, now)
		if e != nil || !ok {
			t.Fatal(ok, e)
		}
	}
	if ok, e := a.store.TakeLogin("api", "client", 2, 3, now); e != nil || ok {
		t.Fatal("IP budget not enforced", e)
	}
	a.Close()
	a, e = New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if ok, e := a.store.TakeLogin("api", "client", 2, 3, now); e != nil || ok {
		t.Fatal("restart reset login budget", e)
	}
	if ok, e := a.store.TakeLogin("api", "other", 2, 3, now); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if ok, e := a.store.TakeLogin("api", "third", 2, 3, now); e != nil || ok {
		t.Fatal("account budget not enforced", e)
	}
	if ok, e := a.store.TakeLogin("admin", "client", 2, 3, now); e != nil || !ok {
		t.Fatal("API attempts blocked admin realm", e)
	}
	if ok, e := a.store.TakeLogin("api", "client", 2, 3, now.Add(time.Minute)); e != nil || !ok {
		t.Fatal("window did not reset", e)
	}
	a.Close()
	if _, e := a.store.TakeLogin("api", "client", 2, 3, now); e == nil {
		t.Fatal("storage failure ignored")
	}
}
func TestAPIRequestRateBudgetsAndConcurrency(t *testing.T) {
	c := testConfig(t)
	c.Security.APIBurst = 2
	c.Security.APIRequestsPerMinute = 60
	a, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	token := approvedToken(t, a)
	for range 2 {
		if w := apiReq(a, token, "GET", "/reader/api/0/user-info", nil); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	w := apiReq(a, token, "GET", "/reader/api/0/user-info", nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code)
	}
	l := newRequestLimiter(c.security())
	now := time.Unix(10000, 0)
	first, second, third := l.allow("a", now), l.allow("a", now), l.allow("a", now)
	if !first || !second || third {
		t.Fatal("burst not enforced")
	}
	if !l.allow("a", now.Add(time.Second)) {
		t.Fatal("tokens did not refill")
	}
	for i := range 10000 {
		l.clients[fmt.Sprint(i)] = tokenBucket{last: now}
	}
	if l.allow("new", now) {
		t.Fatal("unbounded limiter clients")
	}
	if !l.allow("new", now.Add(11*time.Minute)) {
		t.Fatal("stale clients not removed")
	}
	c.Security.APIBurst = 2
	c.Database = filepath.Join(t.TempDir(), "second.db")
	b, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	for _, peer := range []string{"192.0.2.1:1", "192.0.2.2:1", "192.0.2.3:1"} {
		r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info", nil)
		r.RemoteAddr = peer
		r.Header.Set("Authorization", "GoogleLogin auth="+approvedToken(t, b))
		w := httptest.NewRecorder()
		b.APIHandler().ServeHTTP(w, r)
		want := 200
		if peer == "192.0.2.3:1" {
			want = 429
		}
		if w.Code != want {
			t.Fatal("account budget", w.Code)
		}
	}
	a.limiter = newRequestLimiter(Config{}.security())
	for range cap(a.apiSlots) {
		a.apiSlots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info", nil).WithContext(ctx)
	r.Header.Set("Authorization", "GoogleLogin auth="+token)
	a.APIHandler().ServeHTTP(httptest.NewRecorder(), r)
	for range cap(a.apiSlots) {
		<-a.apiSlots
	}
}

func TestArticleResponseBudgetsAreAtomicAndCleanedUp(t *testing.T) {
	a := testApp(t)
	a.config.Security.MaxResponseBytes = 2048
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "one", Content: strings.Repeat("<", 500)}, &gofeed.Item{GUID: "two", Content: "small"})
	token := approvedToken(t, a)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	for _, route := range []string{"stream/items/contents", "stream/contents/reading-list"} {
		w := apiReq(a, token, "POST", "/reader/api/0/"+route, url.Values{"i": {"1", "2"}})
		if w.Code != 413 || strings.Contains(w.Body.String(), `"items"`) {
			t.Fatal("partial/oversized response", route, w.Code, w.Body.String())
		}
		files, e := os.ReadDir(temp)
		if e != nil || len(files) != 0 {
			t.Fatal("response spool leaked", files, e)
		}
	}
	a.config.Security.MaxResponseBytes = 1024
	mustExec(t, a, "UPDATE entries SET content=? WHERE id=1", strings.Repeat("x", 2000))
	if w := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {"1"}}); w.Code != 413 {
		t.Fatal(w.Code)
	}
	a.config.Security.MaxResponseBytes = 64 << 20
	w := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {"2", "1", "999"}})
	if w.Code != 200 || !json.Valid(w.Body.Bytes()) || w.Header().Get("Content-Length") != fmt.Sprint(w.Body.Len()) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {"1", "invalid!"}})
	if w.Code != 400 || strings.Contains(w.Body.String(), `"items"`) {
		t.Fatal("partial invalid batch", w.Code)
	}
	if e := a.articleResponse(httptest.NewRecorder(), func(io.Writer) error { return io.ErrUnexpectedEOF }); e != io.ErrUnexpectedEOF {
		t.Fatal(e)
	}
	if e := a.articleResponse(httptest.NewRecorder(), func(w io.Writer) error { _, e := w.Write([]byte("{}")); return e }); e != nil {
		t.Fatal(e)
	}
	// This is an invalid temp location, not a request to change the user's data path.
	file := filepath.Join(temp, "file")
	if e := os.WriteFile(file, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("TMPDIR", file)
	if e := a.articleResponse(httptest.NewRecorder(), func(io.Writer) error { return nil }); e == nil {
		t.Fatal("temp storage failure ignored")
	}
}
func TestSecurityConfigurationBounds(t *testing.T) {
	for _, change := range []func(*Config){func(c *Config) { c.Security.MaxResponseBytes = -1 }, func(c *Config) { c.Security.MaxResponseBytes = 257 << 20 }, func(c *Config) { c.Security.LoginPerMinute = -1 }, func(c *Config) { c.Security.AccountLoginPerMinute = -1 }, func(c *Config) { c.Security.APIRequestsPerMinute = -1 }, func(c *Config) { c.Security.APIBurst = -1 }, func(c *Config) { c.Security.APIConcurrency = 33 }, func(c *Config) { c.Security.TrustedProxies = []string{"invalid"} }, func(c *Config) { c.Security.TrustedProxies = []string{"0.0.0.0/0"} }} {
		c := testConfig(t)
		change(&c)
		if e := c.Validate(); e == nil {
			t.Fatal("invalid security config accepted")
		}
	}
}
