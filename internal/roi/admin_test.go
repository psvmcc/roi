package roi

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAdminAndOps(t *testing.T) {
	a := testApp(t)
	h := a.AdminHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {"test-password-123"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Subscriptions") {
		t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("POST", "/sync", strings.NewReader("id=0"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("CSRF not enforced")
	}
	for _, path := range []string{"/health", "/metrics"} {
		w = httptest.NewRecorder()
		a.OpsHandler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
	}
}
