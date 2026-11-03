package roi

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPersistentBrowserSessionRestartAndIPChange(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	cookie, csrf := adminSession(t, a)
	original, err := a.loadBrowserSession(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if !cookie.HttpOnly || cookie.MaxAge != 86400 || cookie.Expires.IsZero() {
		t.Fatal(cookie)
	}
	var storedID, storedCSRF string
	if err := a.store.DB.QueryRow("SELECT id,csrf FROM auth_sessions WHERE realm='admin'").Scan(&storedID, &storedCSRF); err != nil {
		t.Fatal(err)
	}
	if storedID != sessionID(cookie.Value) || storedID == cookie.Value || storedCSRF != csrf {
		t.Fatal("invalid persisted browser session")
	}
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := a.loadBrowserSession(cookie.Value)
	if err != nil || restored != original {
		t.Fatal(restored, original, err)
	}
	for _, ip := range []string{"198.51.100.10:1234", "203.0.113.20:4321"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(cookie)
		r.RemoteAddr = ip
		r.Header.Set("User-Agent", "Changed browser")
		w := httptest.NewRecorder()
		a.AdminHandler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var lastIP string
	if err := a.store.DB.QueryRow("SELECT last_ip FROM auth_sessions WHERE id=?", storedID).Scan(&lastIP); err != nil || lastIP != "203.0.113.20" {
		t.Fatal(lastIP, err)
	}
	if w := asyncAdmin(a, cookie, "POST", "/sync", url.Values{"csrf": {csrf}, "id": {"0"}}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := asyncAdmin(a, cookie, "POST", "/sync", url.Values{"csrf": {"wrong"}}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(a, cookie, "POST", "/logout", url.Values{"csrf": {csrf}}); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	if w := asyncAdmin(a, cookie, "GET", "/", nil); w.Code != 401 {
		t.Fatal("logout did not survive restart", w.Code)
	}
}

func TestBrowserCredentialChangesAreRealmSpecific(t *testing.T) {
	for _, change := range []string{"admin username", "admin password", "api username", "api password"} {
		t.Run(change, func(t *testing.T) {
			c := testConfig(t)
			a, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { a.Close() }()
			cookie, _ := adminSession(t, a)
			token := approvedToken(t, a)
			switch change {
			case "admin username":
				c.AdminAuth.Username = "new-admin"
			case "admin password":
				c.AdminAuth.PasswordHash = testConfig(t).AdminAuth.PasswordHash
			case "api username":
				c.APIAuth.Username = "new-reader"
			case "api password":
				c.APIAuth.PasswordHash = testConfig(t).APIAuth.PasswordHash
			}
			a.Close()
			a, err = New(c)
			if err != nil {
				t.Fatal(err)
			}
			adminChanged := strings.HasPrefix(change, "admin")
			_, err = a.loadBrowserSession(cookie.Value)
			if (err == nil) == adminChanged || a.sessionActive(sessionID(token)) == !adminChanged {
				t.Fatal("credential revocation crossed realms", err)
			}
		})
	}
}

func TestBrowserSessionValidationAndStorageFailure(t *testing.T) {
	a := testApp(t)
	cookie, _ := adminSession(t, a)
	for _, value := range []string{"", "short", strings.Repeat("z", 64), strings.Repeat("0", 64)} {
		if _, err := a.loadBrowserSession(value); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(value, err)
		}
	}
	for _, csrf := range []string{"", "short", strings.Repeat("z", 64)} {
		mustExec(t, a, "UPDATE auth_sessions SET csrf=? WHERE id=?", csrf, sessionID(cookie.Value))
		if _, err := a.loadBrowserSession(cookie.Value); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(csrf, err)
		}
	}
	cookie, _ = adminSession(t, a)
	mustExec(t, a, "UPDATE auth_sessions SET realm='api' WHERE id=?", sessionID(cookie.Value))
	if _, err := a.loadBrowserSession(cookie.Value); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross realm", err)
	}
	cookie, _ = adminSession(t, a)
	mustExec(t, a, "UPDATE auth_sessions SET expires=? WHERE id=?", time.Now().Unix()-1, sessionID(cookie.Value))
	if w := asyncAdmin(a, cookie, "GET", "/", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	cookie, _ = adminSession(t, a)
	a.Close()
	if w := adminRequest(a, cookie, "GET", "/", nil); w.Code != 500 {
		t.Fatal(w.Code)
	}
}

func TestBrowserSessionLimitAndAtomicCreation(t *testing.T) {
	a := testApp(t)
	expires := time.Now().Add(time.Hour)
	tokens := make([]string, 100)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("%064x", i+1)
		if err := a.createSession(tokens[i], "admin", expires, nil); err != nil {
			t.Fatal(err)
		}
		mustExec(t, a, "UPDATE auth_sessions SET last_seen=? WHERE id=?", i+1, sessionID(tokens[i]))
	}
	mustExec(t, a, `CREATE TRIGGER fail_browser_insert BEFORE INSERT ON auth_sessions BEGIN SELECT RAISE(ABORT,'disk full'); END`)
	if err := a.createSession(randomToken(), "admin", expires, nil); err == nil {
		t.Fatal("ignored insert failure")
	}
	if !a.sessionActive(sessionID(tokens[0])) {
		t.Fatal("eviction not rolled back")
	}
	// A failed login must not issue a browser cookie.
	r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {"test-password-123"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError || len(w.Result().Cookies()) != 0 {
		t.Fatal(w.Code, w.Result().Cookies())
	}
	mustExec(t, a, "DROP TRIGGER fail_browser_insert")
	if err := a.createSession(randomToken(), "admin", expires, nil); err != nil {
		t.Fatal(err)
	}
	if a.sessionActive(sessionID(tokens[0])) || !a.sessionActive(sessionID(tokens[1])) {
		t.Fatal("wrong session evicted")
	}
	var count int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM auth_sessions WHERE realm='admin' AND revoked=0").Scan(&count); err != nil || count != 100 {
		t.Fatal(count, err)
	}
}

func TestLegacyBrowserSessionsRequireOneLogin(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := adminSession(t, a)
	token := approvedToken(t, a)
	mustExec(t, a, "ALTER TABLE auth_sessions DROP COLUMN csrf")
	mustExec(t, a, "ALTER TABLE feeds DROP COLUMN user_agent; ALTER TABLE feeds DROP COLUMN git_tag_filter; PRAGMA user_version=7")
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.loadBrowserSession(cookie.Value); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if !a.validToken(token) {
		t.Fatal("migration revoked API session")
	}
	newCookie, _ := adminSession(t, a)
	if _, err := a.loadBrowserSession(newCookie.Value); err != nil {
		t.Fatal(err)
	}
}
