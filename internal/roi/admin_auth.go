package roi

import (
	"net/http"
	"time"
)

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if e := adminTemplate.ExecuteTemplate(w, "login.html", nil); e != nil {
			a.requestError(w, r, e)
		}
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Login CSRF: require same-origin browser submissions; non-browser API is on another listener.
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "cross-site login blocked", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if e := r.ParseForm(); e != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !a.allowLogin(r) {
		http.Error(w, "try later", http.StatusTooManyRequests)
		return
	}
	valid, err := checkCredentialsContext(r.Context(), a.config.AdminAuth, r.PostForm.Get("username"), r.PostForm.Get("password"))
	if err != nil {
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if !valid {
		http.Error(w, "invalid credentials", http.StatusForbidden)
		return
	}
	token := randomToken()
	expires := time.Now().Add(24 * time.Hour)
	if err := a.createSession(token, "admin", expires, r); err != nil {
		a.requestError(w, r, err)
		return
	}
	a.touchSession(r, sessionID(token))
	http.SetCookie(w, &http.Cookie{Name: "roi_session", Value: token, Expires: expires, Path: "/", HttpOnly: true, Secure: a.config.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
