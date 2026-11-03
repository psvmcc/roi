package roi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdminImportExportSyncPauseAndLogout(t *testing.T) {
	a := testApp(t)
	c, csrf := adminSession(t, a)
	var body bytes.Buffer
	mp := multipart.NewWriter(&body)
	if e := mp.WriteField("csrf", csrf); e != nil {
		t.Fatal(e)
	}
	f, e := mp.CreateFormFile("opml", "test.opml")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write([]byte(`<opml><body><outline text="Test" xmlUrl="https://example.org/rss"/></body></opml>`)); e != nil {
		t.Fatal(e)
	}
	if e = mp.Close(); e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", "/import", &body)
	r.Header.Set("Content-Type", mp.FormDataContentType())
	r.AddCookie(c)
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	fs, e := a.store.Feeds()
	if e != nil || len(fs) != 1 {
		t.Fatal(fs, e)
	}
	id := fs[0].ID
	w = adminRequest(a, c, "GET", "/export", nil)
	if w.Code != 200 || !strings.HasSuffix(w.Header().Get("Content-Disposition"), `.opml"`) || !strings.Contains(w.Body.String(), "https://example.org/rss") {
		t.Fatal(w.Body.String())
	}
	form := url.Values{"csrf": {csrf}, "id": {fmtInt(id)}}
	mustExec(t, a, "UPDATE feeds SET next_fetch=9999999999 WHERE id=?", id)
	for _, v := range []url.Values{form, {"csrf": {csrf}, "id": {"0"}}} {
		w = adminRequest(a, c, "POST", "/sync", v)
		if w.Code != 303 {
			t.Fatal(w.Code)
		}
	}
	for range 2 {
		w = adminRequest(a, c, "POST", "/toggle", form)
		if w.Code != 303 {
			t.Fatal(w.Code)
		}
	}
	w = adminRequest(a, c, "GET", "/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Test") {
		t.Fatal(w.Body.String())
	}
	w = adminRequest(a, c, "POST", "/logout", url.Values{"csrf": {csrf}})
	if w.Code != 303 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal(w.Code)
	}
	if w = adminRequest(a, c, "GET", "/", nil); w.Code != 303 {
		t.Fatal("logout did not invalidate session")
	}
}
func fmtInt(n int64) string { return strconv.FormatInt(n, 10) }

func TestAdminAuthenticationAndRequestBoundaries(t *testing.T) {
	a := testApp(t)
	a.config.SecureCookies = true
	c, csrf := adminSession(t, a)
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure session cookie")
	}
	for _, path := range []string{"/assets/admin.css", "/assets/theme.js"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := adminRequest(a, nil, method, path, nil)
			if w.Code != 200 {
				t.Fatal(path, method, w.Code)
			}
		}
		if w := adminRequest(a, nil, "POST", path, nil); w.Code != 405 {
			t.Fatal(w.Code)
		}
	}
	for _, path := range []string{"/assets/../config.yaml", "/assets/missing.js", "/missing"} {
		if w := adminRequest(a, c, "GET", path, nil); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	if w := adminRequest(a, c, "DELETE", "/", nil); w.Code != 405 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/sync", "/toggle"} {
		if w := adminRequest(a, c, "POST", path, url.Values{"csrf": {csrf}, "id": {"invalid"}}); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if w := adminRequest(a, c, "POST", "/missing", url.Values{"csrf": {csrf}}); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(a, c, "POST", "/import", url.Values{"csrf": {csrf}}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/sync", strings.NewReader("csrf=%zz"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(c)
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(a, nil, "PUT", "/login", nil); w.Code != 405 {
		t.Fatal(w.Code)
	}
	r = httptest.NewRequest("POST", "/login", strings.NewReader("username=admin&password=wrong"))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(a, nil, "POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong"}}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	mustExec(t, a, "UPDATE auth_sessions SET expires=? WHERE id=?", time.Now().Add(-time.Second).Unix(), sessionID(c.Value))
	if w := adminRequest(a, c, "GET", "/", nil); w.Code != 303 {
		t.Fatal("expired session accepted")
	}
	unknown := &http.Cookie{Name: "roi_session", Value: "unknown"}
	if w := adminRequest(a, unknown, "GET", "/", nil); w.Code != 303 {
		t.Fatal(w.Code)
	}
	for range 10 {
		a.allowLogin(httptest.NewRequest("POST", "/login", nil))
	}
	if w := adminRequest(a, nil, "POST", "/login", nil); w.Code != 429 {
		t.Fatal(w.Code)
	}
}

func TestAdminInvalidOPMLAndStorageFailure(t *testing.T) {
	a := testApp(t)
	c, csrf := adminSession(t, a)
	for _, raw := range []string{"<broken>", `<opml><body><outline xmlUrl="file:///etc/passwd"/></body></opml>`} {
		var b bytes.Buffer
		mp := multipart.NewWriter(&b)
		_ = mp.WriteField("csrf", csrf)
		f, _ := mp.CreateFormFile("opml", "invalid.opml")
		_, _ = f.Write([]byte(raw))
		_ = mp.Close()
		r := httptest.NewRequest("POST", "/import", &b)
		r.AddCookie(c)
		r.Header.Set("Content-Type", mp.FormDataContentType())
		w := httptest.NewRecorder()
		a.AdminHandler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/", "/delete?id=1", "/export"} {
		w := adminRequest(a, c, "GET", path, nil)
		if w.Code != 500 {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/disable", "/delete", "/enable", "/toggle", "/sync"} {
		w := adminRequest(a, c, "POST", path, url.Values{"id": {"1"}, "csrf": {csrf}})
		if w.Code != 500 {
			t.Fatal(path, w.Code)
		}
	}
}
