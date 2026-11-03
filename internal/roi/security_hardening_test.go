package roi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
	"go.yaml.in/yaml/v3"
)

func TestConfiguredRealIPHeaderTrustAndFallback(t *testing.T) {
	a := testApp(t)
	a.config.Security.RealIPHeader = "CF-Connecting-IP"
	a.config.Security.TrustedProxies = []string{"127.0.0.1", "::1"}
	for _, tc := range []struct {
		peer   string
		values []string
		want   string
	}{
		{"127.0.0.1:10", []string{"203.0.113.7"}, "203.0.113.7"},
		{"192.0.2.1:10", []string{"203.0.113.7"}, "192.0.2.1"},
		{"[::1]:10", []string{"2001:db8::7"}, "2001:db8::7"},
		{"127.0.0.1:10", nil, "127.0.0.1"},
		{"127.0.0.1:10", []string{"bad"}, "127.0.0.1"},
		{"127.0.0.1:10", []string{"203.0.113.7, 192.0.2.2"}, "127.0.0.1"},
		{"127.0.0.1:10", []string{"203.0.113.7", "192.0.2.2"}, "127.0.0.1"},
		{"127.0.0.1:10", []string{"::ffff:203.0.113.7"}, "203.0.113.7"},
		{"127.0.0.1:10", []string{strings.Repeat("x", 257)}, "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", "198.51.100.8")
		r.Header.Set("X-Real-IP", "198.51.100.9")
		for _, v := range tc.values {
			r.Header.Add("CF-Connecting-IP", v)
		}
		if got := a.clientIP(r); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	a.config.Security.RealIPHeader = "X-Forwarded-For"
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:10"
	r.Header.Set("X-Real-IP", "203.0.113.8")
	if got := a.clientIP(r); got != "127.0.0.1" {
		t.Fatal("fell back to an unselected header", got)
	}
	for _, name := range []string{"Bad Header", "Bad:Header", "\r\nCookie", strings.Repeat("x", 65)} {
		c := testConfig(t)
		c.Security.RealIPHeader = name
		if err := c.Validate(); err == nil {
			t.Fatal(name)
		}
	}
	c := testConfig(t)
	c.Security.RealIPHeader = "CF-Connecting-IP"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredRealIPHeaderReachesSessionsAndSecurityEvents(t *testing.T) {
	a := testApp(t)
	a.config.Security.RealIPHeader = "CF-Connecting-IP"
	a.config.Security.TrustedProxies = []string{"127.0.0.1"}
	token := approvedToken(t, a)
	request := func(peer, client, auth string) int {
		r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info", nil)
		r.RemoteAddr = peer
		r.Header.Set("CF-Connecting-IP", client)
		r.Header.Set("Authorization", "GoogleLogin auth="+auth)
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct{ peer, header, want string }{
		{"127.0.0.1:1000", "203.0.113.10", "203.0.113.10"},
		{"192.0.2.20:1000", "203.0.113.10", "192.0.2.20"},
	} {
		if code := request(tc.peer, tc.header, token); code != 200 {
			t.Fatal(code)
		}
		var last string
		if err := a.store.DB.QueryRow("SELECT last_ip FROM auth_sessions WHERE id=?", sessionID(token)).Scan(&last); err != nil || last != tc.want {
			t.Fatal("session IP", last, err)
		}
	}
	if code := request("127.0.0.1:1000", "203.0.113.11", "forged"); code != 401 {
		t.Fatal(code)
	}
	events, err := a.securityHistory()
	if err != nil || len(events) == 0 || events[0].IP != "203.0.113.11" {
		t.Fatal("security event IP", events, err)
	}
}

func TestGlobalAdmissionRejectsBeforeAuthenticationAndSamplesAudit(t *testing.T) {
	c := testConfig(t)
	c.Security.APIBurst = 2
	c.Security.APIRequestsPerMinute = 1
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info", nil)
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1", i+1)
		r.Header.Set("Authorization", "GoogleLogin auth=v2.0."+strings.Repeat("a", 64)+"."+strings.Repeat("b", 64))
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
		want := 401
		if i == 2 {
			want = 429
		}
		if w.Code != want {
			t.Fatal(i, w.Code)
		}
	}
	gate := auditGate{}
	now := time.Now()
	e := APIRequest{Realm: "api", IP: "192.0.2.1", Status: 401}
	if !gate.allow(e, now) || gate.allow(e, now) || !gate.allow(e, now.Add(31*time.Second)) {
		t.Fatal("audit deduplication failed")
	}
	loginGate := auditGate{}
	denial := APIRequest{Realm: "api", IP: "192.0.2.2", Status: 403, Path: "/greader/reader/api/0/edit-tag"}
	if !loginGate.allow(denial, now) {
		t.Fatal("first mutation denial suppressed")
	}
	denial.Path = "/greader/accounts/ClientLogin"
	if !loginGate.allow(denial, now) || loginGate.allow(denial, now) {
		t.Fatal("login failures must have a separate sampled category")
	}
	denied := 0
	for i := 0; i < 40; i++ {
		e.IP = fmt.Sprint(i)
		if !gate.allow(e, now.Add(31*time.Second)) {
			denied++
		}
	}
	if denied == 0 {
		t.Fatal("global audit write budget missing")
	}
	e.Status = 200
	if !gate.allow(e, now) {
		t.Fatal("successful security event suppressed")
	}
	for i := 0; i < 4096; i++ {
		gate.seen[fmt.Sprint(i)] = now
	}
	e.Status = 403
	if gate.allow(e, now) {
		t.Fatal("unbounded audit keys")
	}
	token := a.issueToken()
	if !a.tokenSignatureValid(token) || a.tokenSignatureValid(token+"bad") {
		t.Fatal("signature validation")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.validToken(token) || a.tokenSignatureValid("invalid") {
		t.Fatal("closed or invalid token accepted")
	}
}

func TestCacheBoundsHeadersRowsAndOutboundResponseHeaders(t *testing.T) {
	a := testApp(t)
	for _, entry := range []gitCacheEntry{{ETag: strings.Repeat("x", 4097)}, {Modified: strings.Repeat("x", 129)}} {
		if err := a.poller.cacheGit(t.Context(), "large", entry); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM git_http_cache").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	mustExec(t, a, `WITH RECURSIVE numbers(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM numbers WHERE x<2100) INSERT INTO git_http_cache SELECT CAST(x AS TEXT),'etag','',X'',0,? FROM numbers`, time.Now().Unix())
	if err := a.poller.cacheGit(t.Context(), "new", gitCacheEntry{ETag: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB.QueryRow("SELECT count(*) FROM git_http_cache").Scan(&count); err != nil || count > gitCacheEntries {
		t.Fatal(count, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("x", 70<<10))
		fmt.Fprint(w, "[]")
	}))
	defer server.Close()
	budget := gitBudget{Limit: 1024}
	if _, err := a.poller.gitGet(t.Context(), server.URL, "", &budget); err == nil {
		t.Fatal("oversized response headers accepted")
	}
}

func TestExplicitOutboundNetworksAndMetadataDeny(t *testing.T) {
	c := testConfig(t)
	c.Poll.AllowPrivate = false
	c.Poll.AllowedNetworks = []string{"10.20.0.0/24", "127.0.0.1", "::1"}
	for ip, want := range map[string]bool{"8.8.8.8": true, "10.20.0.5": true, "10.21.0.5": false, "127.0.0.1": true, "::1": true, "169.254.169.254": false, "100.100.100.200": false, "fd00:ec2::254": false, "::": false, "224.0.0.1": false, "64:ff9b::a9fe:a9fe": false, "2002:a9fe:a9fe::": false} {
		if got := c.outboundIPAllowed(netip.MustParseAddr(ip)); got != want {
			t.Fatal(ip, got)
		}
	}
	c.Poll.AllowPrivate = true
	c.Poll.AllowedNetworks = []string{"169.254.0.0/16", "fd00::/8"}
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "fd00:ec2::23", "168.63.129.16", "100.100.100.200", "64:ff9b::a83f:8110", "2002:a83f:8110::"} {
		if c.outboundIPAllowed(netip.MustParseAddr(ip)) {
			t.Fatal("metadata allowed", ip)
		}
	}
	for _, raw := range []string{"bad", "0.0.0.0/0", "::/0", "fe80::1%en0"} {
		c.Poll.AllowedNetworks = []string{raw}
		if err := c.Validate(); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestParserLimitsAndCancellation(t *testing.T) {
	for _, raw := range []string{
		"<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss><channel><title>Caf\xe9</title></channel></rss>",
		"<rss><channel><title>A &nbsp; B</title></channel></rss>",
	} {
		if err := checkFeedDocument([]byte(raw)); err != nil {
			t.Fatal("valid legacy feed rejected", err)
		}
		if _, err := gofeed.NewParser().ParseString(raw); err != nil {
			t.Fatal("feed parser compatibility", err)
		}
	}
	tests := []struct {
		name  string
		check func([]byte) error
		body  string
	}{
		{"XML depth", checkFeedDocument, strings.Repeat("<x>", 129) + strings.Repeat("</x>", 129)},
		{"XML attributes", checkFeedDocument, "<r " + strings.Repeat("a='b' ", 65) + "/>"},
		{"feed items", checkFeedDocument, "<rss>" + strings.Repeat("<item/>", 10001) + "</rss>"},
		{"XML tokens", checkFeedDocument, "<r>" + strings.Repeat("<x/>", 100001) + "</r>"},
		{"JSON depth", checkJSONDocument, strings.Repeat("[", 129) + strings.Repeat("]", 129)},
		{"JSON tokens", checkJSONDocument, "[" + strings.Repeat("0,", 200001) + "0]"},
		{"HTML tokens", checkHTMLDocument, strings.Repeat("<br>", 200001)},
		{"HTML text", checkHTMLDocument, strings.Repeat("x", (4<<20)+1)},
		{"YAML depth", checkYAMLDocument, strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)},
		{"YAML width", checkYAMLDocument, strings.Repeat(",", 200001)},
		{"YAML aliases", checkYAMLDocument, "a: &a [1]\nb: *a\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.check([]byte(tc.body)); err == nil {
				t.Fatal("limit missing")
			}
		})
	}
	for _, body := range []string{`{"version":"https://jsonfeed.org/version/1","items":[]}`, `<rss><channel/></rss>`} {
		if err := checkFeedDocument([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	g := newParseGate(SecurityConfig{ParseConcurrency: 1, ParseMemoryBytes: 1024})
	if err := g.acquire(t.Context(), 512); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := g.acquire(ctx, 512); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := g.acquire(t.Context(), 2048); err == nil {
		t.Fatal("oversized admission")
	}
	g.release(512)
	if err := g.acquire(t.Context(), 512); err != nil {
		t.Fatal(err)
	}
	g.release(512)
	hash, err := HashPassword([]byte("test-password-123"))
	if err != nil {
		t.Fatal(err)
	}
	argonSlots <- struct{}{}
	argonSlots <- struct{}{}
	defer func() { <-argonSlots; <-argonSlots }()
	for _, h := range []string{hash, testConfig(t).AdminAuth.PasswordHash} {
		if _, err := VerifyPasswordContext(ctx, h, []byte("test-password-123")); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}

func TestPrivateFilePermissionsPolicies(t *testing.T) {
	c := testConfig(t)
	c.Security.FilePermissions = "strict"
	if err := os.WriteFile(c.Database, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.Database, 0644); err != nil {
		t.Fatal(err)
	}
	if a, err := New(c); err == nil {
		a.Close()
		t.Fatal("insecure DB accepted in strict mode")
	}
	if _, err := CompactDatabase(t.Context(), c); err == nil {
		t.Fatal("compaction bypassed strict permissions")
	}
	if err := checkPrivateFile(c.Database, "warn"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.Database, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(c.Database), 0755); err != nil {
		t.Fatal(err)
	}
	if a, err := New(c); err == nil {
		a.Close()
		t.Fatal("insecure storage directory accepted")
	}
	if err := os.Chmod(filepath.Dir(c.Database), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(c.Database), "config.yaml")
	raw, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, raw, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(file); err == nil {
		t.Fatal("insecure configuration accepted")
	}
	if err = os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(file); err != nil {
		t.Fatal(err)
	}
	if err = checkPrivateFile(filepath.Dir(file), "strict"); err == nil {
		t.Fatal("directory accepted as private file")
	}
}

func TestSafeRestoreRevokesSessionsAndOfflineRotationLocks(t *testing.T) {
	a := testApp(t)
	c := a.config
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "persist", Title: "Saved", Content: "Body"})
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1")
	token := approvedToken(t, a)
	cookie, _ := adminSession(t, a)
	directory := t.TempDir()
	source := filepath.Join(directory, "snapshot.sqlite")
	if err := writeBackup(t.Context(), c.Database, directory, source); err != nil {
		t.Fatal(err)
	}
	if err := RotateStoppedAPIKey(c); err == nil {
		t.Fatal("rotation bypassed running instance lock")
	}
	if _, err := RestoreDatabase(t.Context(), c, source); err == nil {
		t.Fatal("restore bypassed running instance lock")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(directory, "bad.sqlite")
	if err := os.WriteFile(bad, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(c.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RestoreDatabase(t.Context(), c, bad); err == nil {
		t.Fatal("corrupt backup restored")
	}
	after, err := os.ReadFile(c.Database)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed restore changed database", err)
	}
	if _, err = RestoreDatabase(t.Context(), c, c.Database); err == nil {
		t.Fatal("self restore accepted")
	}
	previous, err := RestoreDatabase(t.Context(), c, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(previous, "database.sqlite")); err != nil {
		t.Fatal(err)
	}
	b, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.validToken(token) {
		t.Fatal("restored API token still valid")
	}
	if _, err = b.loadBrowserSession(cookie.Value); err == nil {
		t.Fatal("restored browser still valid")
	}
	var read, starred bool
	if err = b.store.DB.QueryRow("SELECT is_read,starred FROM entries WHERE feed_id=?", id).Scan(&read, &starred); err != nil || !read || !starred {
		t.Fatal(read, starred, err)
	}
	assertFeedAggregates(t, b)
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	if err = RotateStoppedAPIKey(c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = RestoreDatabase(ctx, c, source); err == nil {
		t.Fatal("canceled restore succeeded")
	}
	if err = copyPrivateFile(ctx, source, filepath.Join(directory, "canceled.sqlite")); err == nil {
		t.Fatal("canceled copy succeeded")
	}
	if err = syncDirectory(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing directory synced")
	}
}

func TestSafeRestorePreservesCorruptDestinationAndSidecars(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "snapshot", Content: "Saved"})
	snapshot := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := writeBackup(t.Context(), a.config.Database, filepath.Dir(snapshot), snapshot); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(c.Database+suffix, []byte("original"+suffix), 0600); err != nil {
			t.Fatal(err)
		}
	}
	previous, err := RestoreDatabase(t.Context(), c, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(previous, "database.sqlite"+suffix))
		if err != nil || string(data) != "original"+suffix {
			t.Fatal("original recovery files lost", suffix, string(data), err)
		}
		if suffix != "" {
			if _, err := os.Stat(c.Database + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("stale sidecar left beside restored database", suffix, err)
			}
		}
	}
	newApp, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer newApp.Close()
	var content string
	if err = newApp.store.DB.QueryRow("SELECT content FROM entries WHERE feed_id=?", id).Scan(&content); err != nil || content != "Saved" {
		t.Fatal(content, err)
	}
}
