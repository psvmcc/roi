package roi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAdminOriginAndCSRFBoundaries(t *testing.T) {
	a := testApp(t)
	c, csrf := adminSession(t, a)
	for _, origin := range []string{"null", "https://attacker.example", "http://example.com:8080", "http://user@example.com", "http://example.com/path", "http://example.com?x=1"} {
		for _, path := range []string{"/login", "/disable"} {
			r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"csrf": {csrf}, "id": {"1"}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", origin)
			r.AddCookie(c)
			w := httptest.NewRecorder()
			a.AdminHandler().ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal(origin, path, w.Code)
			}
		}
	}
	for _, origin := range []string{"", "http://example.com"} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Origin", origin)
		if !a.sameOrigin(r) {
			t.Fatal(origin)
		}
	}
	r := httptest.NewRequest("POST", "/", nil)
	a.config.SecureCookies = true
	httpsReq := httptest.NewRequest("POST", "/", nil)
	httpsReq.Header.Set("Origin", "https://example.com")
	if !a.sameOrigin(httpsReq) {
		t.Fatal("HTTPS origin rejected")
	}
	httpsReq.Header.Set("Origin", "http://example.com")
	if a.sameOrigin(httpsReq) {
		t.Fatal("cross-scheme origin accepted")
	}
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if a.sameOrigin(r) {
		t.Fatal("cross-site Fetch Metadata accepted")
	}
}
func TestCredentialsMustNotComeFromURL(t *testing.T) {
	a := testApp(t)
	w := apiReq(a, "", "POST", "/accounts/ClientLogin?Email=reader&Passwd=test-password-123", nil)
	if w.Code != 403 {
		t.Fatal("query credentials accepted", w.Code)
	}
	w = adminRequest(a, nil, "POST", "/login?username=admin&password=test-password-123", nil)
	if w.Code != 403 {
		t.Fatal("query credentials accepted", w.Code)
	}
}
func TestEmbeddedPrivateIPv4IsBlocked(t *testing.T) {
	for _, raw := range []string{"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b:1::1", "2002:7f00:1::", "2001::1", "::127.0.0.1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("private tunnel address allowed", raw)
		}
	}
	for _, raw := range []string{"64:ff9b::808:808", "2002:808:808::"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("public translated address blocked", raw)
		}
	}
}
func TestFeedErrorURLsRemainVisible(t *testing.T) {
	a := testApp(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	id := addFeed(t, a, server.URL+"/private?token=secret")
	a.poller.fetch(t.Context(), id)
	var stored string
	if e := a.store.DB.QueryRow("SELECT error FROM feeds WHERE id=?", id).Scan(&stored); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(stored, "/private?token=secret") {
		t.Fatal("persisted feed error omitted URL", stored)
	}
}
func TestAdminEscapesUntrustedFeedFields(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	mustExec(t, a, "UPDATE feeds SET title=?,site=?,error=? WHERE id=?", `<script>alert(1)</script>`, `javascript:alert(1)`, `<img src=x onerror=alert(1)>`, id)
	c, _ := adminSession(t, a)
	w := adminRequest(a, c, "GET", "/", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>alert") || strings.Contains(w.Body.String(), "<img src=x") {
		t.Fatal("untrusted feed HTML rendered")
	}
	for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Cache-Control": "no-store"} {
		if w.Header().Get(k) != want {
			t.Fatal(k)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("missing CSP")
	}
}

func FuzzItemIDRoundTrip(f *testing.F) {
	for _, s := range []string{"1", "26", "000000000000001a", "tag:google.com,2005:reader/item/000000000000001a", "0x1a", "invalid", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		id, e := parseEntryID(s)
		if e != nil {
			return
		}
		if id <= 0 {
			t.Fatal("nonpositive ID accepted")
		}
		got, e := parseEntryID(entryID(id))
		if e != nil || got != id {
			t.Fatal("accepted ID cannot round trip", s, id, got, e)
		}
	})
}

func TestAPIUserAgentLogIsBounded(t *testing.T) {
	a := testApp(t)
	for _, agent := range []string{strings.Repeat("x", 4096), strings.Repeat("é", 256), strings.Repeat(string([]byte{0xff}), 1024)} {
		r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info", nil)
		r.Header.Set("User-Agent", agent)
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
		events, err := a.securityHistory()
		if err != nil || len(events) == 0 {
			t.Fatal(err)
		}
		client := events[0].Client
		if len(client) > 256 || !utf8.ValidString(client) {
			t.Fatal("unbounded or invalid user agent", len(client))
		}
	}
}
