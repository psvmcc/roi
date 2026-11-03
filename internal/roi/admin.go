package roi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/psvmcc/roi/internal/web"
)

func (a *App) AdminHandler() http.Handler {
	return a.logAdmin(a.wrap("admin", http.HandlerFunc(a.admin)))
}

func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	if r.URL.Path == "/favicon.ico" {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Redirect(w, r, "/assets/favicon.svg", http.StatusTemporaryRedirect)
		return
	}
	if r.URL.Path == "/assets/favicon.svg" || r.URL.Path == "/assets/admin.css" || r.URL.Path == "/assets/theme.js" || r.URL.Path == "/assets/admin.js" {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, e := web.Files.ReadFile(r.URL.Path[1:])
		if e != nil {
			a.requestError(w, r, e)
			return
		}
		http.ServeContent(w, r, r.URL.Path, time.Time{}, bytes.NewReader(data))
		return
	}
	if r.Method == "POST" && !a.sameOrigin(r) {
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/login" {
		a.login(w, r)
		return
	}
	cookie, e := r.Cookie("roi_session")
	if e != nil {
		a.adminUnauthorized(w, r)
		return
	}
	s, err := a.loadBrowserSession(cookie.Value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.adminUnauthorized(w, r)
		} else {
			a.requestError(w, r, err)
		}
		return
	}
	a.touchSession(r, sessionID(cookie.Value))
	if r.Method == "POST" {
		if r.URL.Path == "/import" || r.URL.Path == "/import-roi" || r.URL.Path == "/preview-import" {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if err := a.poller.resources.acquire(ctx, 16<<20); err != nil {
				rateLimited(w)
				return
			}
			defer a.poller.resources.release(16 << 20)
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		if r.URL.Path == "/import" || r.URL.Path == "/import-roi" || r.URL.Path == "/preview-import" {
			e = r.ParseMultipartForm(8 << 20)
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
		} else {
			e = r.ParseForm()
		}
		if e != nil {
			http.Error(w, "invalid form (maximum 8 MiB)", http.StatusBadRequest)
			return
		}
		if !hmac.Equal([]byte(s.CSRF), []byte(r.FormValue("csrf"))) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		a.adminAction(w, r, cookie)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/delete" {
		a.deleteConfirmation(w, r, s.CSRF)
		return
	}
	if r.URL.Path == "/export-roi" {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="roi-`+time.Now().UTC().Format("20060102T150405.000000000Z")+`.roi"`)
		if err := a.store.ExportROI(w); err != nil {
			a.requestError(w, r, err)
		}
		return
	}
	if r.URL.Path == "/export" {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="roi-`+time.Now().UTC().Format("20060102T150405.000000000Z")+`.opml"`)
		if e = a.store.Export(w); e != nil {
			a.requestError(w, r, e)
		}
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	a.adminDashboard(w, r, cookie, s.CSRF)
}

func (a *App) adminStoreError(w http.ResponseWriter, r *http.Request, e error) {
	switch {
	case errors.Is(e, ErrFeedNotFound):
		http.Error(w, e.Error(), http.StatusNotFound)
	case errors.Is(e, ErrFeedEnabled), errors.Is(e, ErrFeedDisabled):
		http.Error(w, e.Error(), http.StatusConflict)
	default:
		a.requestError(w, r, e)
	}
}

func (a *App) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	scheme := "http"
	if a.config.SecureCookies || r.TLS != nil {
		scheme = "https"
	}
	u, e := url.Parse(origin)
	return e == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func wantsAdminJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (a *App) adminUnauthorized(w http.ResponseWriter, r *http.Request) {
	if wantsAdminJSON(r) || r.Header.Get("X-ROI-Request") == "async" {
		http.Error(w, "session expired", http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
