package roi

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSecurityEventsFailedLoginsAndOrdinaryRequestLogs(t *testing.T) {
	a := testApp(t)
	a.config.Debug = true
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	token := approvedToken(t, a)
	for _, tc := range []struct {
		realm, path, ip string
		values          url.Values
	}{
		{"admin", "/login", "198.51.100.20:1234", url.Values{"username": {"admin"}, "password": {"private-password-attempt"}}},
		{"api", "/greader/accounts/ClientLogin", "198.51.100.21:4321", url.Values{"Email": {"reader"}, "Passwd": {"private-password-attempt"}}},
	} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.values.Encode()))
		r.RemoteAddr = tc.ip
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("User-Agent", "Test client")
		w := httptest.NewRecorder()
		if tc.realm == "admin" {
			a.AdminHandler().ServeHTTP(w, r)
		} else {
			a.APIHandler().ServeHTTP(w, r)
		}
		if w.Code != 403 {
			t.Fatal(tc.realm, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info?private-query", nil)
	r.Header.Set("Authorization", "GoogleLogin auth="+token)
	r.RemoteAddr = "198.51.100.22:5678"
	w := httptest.NewRecorder()
	a.APIHandler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	events, err := a.securityHistory()
	if err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
	for _, event := range events {
		if event.Kind != "Login failed" || event.Status != 403 || event.Client != "Test client" || !strings.HasPrefix(event.IP, "198.51.100.2") {
			t.Fatal(event)
		}
	}
	b, _ := json.Marshal(events)
	for _, secret := range []string{token, "private-password-attempt", "private-query"} {
		if strings.Contains(string(b), secret) || strings.Contains(logs.String(), secret) {
			t.Fatal("secret leaked", secret)
		}
	}
	if !strings.Contains(logs.String(), "198.51.100.22") || !strings.Contains(logs.String(), "/greader/reader/api/0/user-info") {
		t.Fatal("ordinary request missing from server log")
	}
	cookie, _ := adminSession(t, a)
	page := adminRequest(a, cookie, "GET", "/", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Login failed") || strings.Contains(page.Body.String(), "Recent Reader API requests") || strings.Contains(page.Body.String(), "Session history and admin events") || strings.Contains(page.Body.String(), "data-session-history") {
		t.Fatal("obsolete panels or missing security events")
	}
}

func TestSecurityEventKinds(t *testing.T) {
	for _, tc := range []struct {
		realm, method, path string
		status              int
		want                string
	}{
		{"admin", "POST", "/login", 403, "Login failed"}, {"api", "POST", "/greader/accounts/ClientLogin", 403, "Login failed"},
		{"admin", "POST", "/login", 500, "Login unavailable"}, {"admin", "POST", "/login", 400, "Login request rejected"},
		{"api", "POST", "/greader/accounts/ClientLogin", 200, "Device awaiting approval"}, {"admin", "POST", "/login", 303, "Login succeeded"},
		{"api", "GET", "/anything", 429, "Rate limit exceeded"}, {"api", "GET", "/anything", 401, "Authentication rejected"},
		{"admin", "POST", "/rename", 403, "Access denied"}, {"admin", "GET", "/", 200, ""},
		{"admin", "POST", "/approve-session", 200, "Device approved"}, {"admin", "POST", "/revoke-session", 200, "Session revoked"},
		{"admin", "POST", "/revoke-api-tokens", 303, "API tokens revoked"}, {"admin", "POST", "/logout", 303, "Signed out"},
		{"admin", "POST", "/approve-session", 400, "Security action failed"}, {"admin", "POST", "/rename", 200, ""},
	} {
		if got := securityEventKind(APIRequest{Realm: tc.realm, Method: tc.method, Path: tc.path, Status: tc.status}); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}

func TestSecurityHistoryFiltersLegacyRequestsAndPersists(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, `INSERT INTO auth_events(created,realm,session_id,method,path,ip,client,status) VALUES(1,'api','','GET','/reader/api/0/user-info','127.0.0.1','',200),(2,'admin','','POST','/login','198.51.100.30','',403)`)
	events, err := a.securityHistory()
	if err != nil || len(events) != 1 || events[0].Kind != "Login failed" {
		t.Fatal(events, err)
	}
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var count int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM auth_events").Scan(&count); err != nil || count != 1 {
		t.Fatal("legacy ordinary requests retained", count, err)
	}
	events, err = a.securityHistory()
	if err != nil || len(events) != 1 || events[0].IP != "198.51.100.30" {
		t.Fatal("security event lost on restart", events, err)
	}
}

func TestFaviconPublicAssetAndLoginCSP(t *testing.T) {
	a := testApp(t)
	for _, method := range []string{"GET", "HEAD", "POST"} {
		w := adminRequest(a, nil, method, "/assets/favicon.svg", nil)
		if method == "POST" {
			if w.Code != 405 {
				t.Fatal(w.Code)
			}
			continue
		}
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "image/svg+xml") {
			t.Fatal(w.Code, w.Header())
		}
		if method == "GET" {
			var doc struct{ XMLName xml.Name }
			if err := xml.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc.XMLName.Local != "svg" {
				t.Fatal("invalid SVG", err)
			}
		}
	}
	w := adminRequest(a, nil, "GET", "/favicon.ico", nil)
	if w.Code != 307 || w.Header().Get("Location") != "/assets/favicon.svg" {
		t.Fatal(w.Code)
	}
	w = adminRequest(a, nil, "POST", "/favicon.ico", nil)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	w = adminRequest(a, nil, "GET", "/login", nil)
	if !strings.Contains(w.Body.String(), `rel="icon"`) || !strings.Contains(w.Header().Get("Content-Security-Policy"), "img-src 'self'") {
		t.Fatal("favicon unavailable under login CSP")
	}
}

func TestRejectedBackgroundRefreshIsSecurityEvent(t *testing.T) {
	a := testApp(t)
	w := asyncAdmin(a, nil, "GET", "/?fragment=1", nil)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	events, err := a.securityHistory()
	if err != nil || len(events) != 1 || events[0].Kind != "Authentication rejected" {
		t.Fatal(events, err)
	}
}
