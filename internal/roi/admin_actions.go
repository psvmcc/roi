package roi

import (
	"net/http"
	"strconv"
	"strings"
)

func adminActionMessage(path string) string {
	if path == "/device-feeds" {
		return "Device subscription visibility updated. Sync the Reader client."
	}
	if path == "/import-roi" {
		return "Subscriptions imported. Existing URLs were skipped."
	}
	if path == "/git-source" {
		return "Git source saved. It will be synced into GIT_Releases."
	}
	if path == "/approve-session" {
		return "Device approved. Sync the Reader client to load subscriptions."
	}
	if path == "/rename-device" {
		return "Device name saved."
	}
	if path == "/revoke-session" {
		return "Session revoked."
	}
	switch path {
	case "/rss-settings":
		return "RSS settings saved."
	case "/sync":
		return "Sync queued."
	case "/disable":
		return "Subscription disabled."
	case "/enable":
		return "Subscription enabled."
	case "/delete":
		return "Subscription deleted."
	case "/reset":
		return "Feed reset. A fresh sync is scheduled."
	case "/rename":
		return "Subscription renamed."
	case "/import":
		return "Subscriptions imported."
	case "/toggle":
		return "Polling state updated."
	case "/revoke-api-tokens":
		return "Reader API tokens revoked."
	}
	return "Saved."
}

func (a *App) adminAction(w http.ResponseWriter, r *http.Request, cookie *http.Cookie) {
	var e error
	switch r.URL.Path {
	case "/device-feeds":
		hidden := r.FormValue("hidden")
		if hidden != "0" && hidden != "1" {
			http.Error(w, "invalid visibility", http.StatusBadRequest)
			return
		}
		if err := a.setDeviceFeedsHidden(r.FormValue("session"), hidden == "1"); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
	case "/reset":
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if r.FormValue("confirm") != "reset" {
			http.Error(w, "reset confirmation required", http.StatusBadRequest)
			return
		}
		a.poller.Cancel(id)
		if err = a.store.ResetFeed(id); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
		a.poller.Wake()
	case "/preview-import":
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "choose a subscriptions file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		preview, err := a.store.PreviewImport(file, r.FormValue("format"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = jsonReply(w, preview)
		return
	case "/rss-settings":
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if err = validateUserAgent(r.FormValue("user_agent")); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
		a.poller.Cancel(id)
		if err = a.store.SetFeedUserAgent(id, r.FormValue("user_agent")); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
		a.poller.Wake()
	case "/git-source":
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil || id < 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		raw, provider, icon := r.FormValue("repository"), r.FormValue("provider"), ""
		if provider == "" {
			provider = "auto"
		}
		if id > 0 {
			err = a.store.DB.QueryRow("SELECT url,git_provider,icon_url FROM feeds WHERE id=? AND git_provider<>''", id).Scan(&raw, &provider, &icon)
			if err != nil {
				a.adminStoreError(w, r, ErrFeedNotFound)
				return
			}
		}
		source, err := parseGitSource(raw, provider, r.FormValue("mode"), r.FormValue("include_preview") == "1")
		if err == nil {
			source.TagFilter = strings.TrimSpace(r.FormValue("tag_filter"))
			err = validateTagFilter(source.TagFilter)
		}
		if err == nil && id == 0 {
			source, err = a.poller.detectGit(r.Context(), source)
		}
		if err != nil {
			a.adminStoreError(w, r, clientError(err.Error()))
			return
		}
		if id > 0 {
			source.Icon = icon
			a.poller.Cancel(id)
		}
		if err = a.store.SaveGitSource(id, source); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
		a.poller.Wake()
	case "/approve-session", "/rename-device":
		if err := a.nameSession(r.FormValue("session"), r.FormValue("device_name"), r.URL.Path == "/approve-session"); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
	case "/revoke-browser-sessions":
		if err := a.revokeBrowsers(); err != nil {
			a.requestError(w, r, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "roi_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.config.SecureCookies, SameSite: http.SameSiteStrictMode})
		if wantsAdminJSON(r) {
			_ = jsonReply(w, map[string]any{"ok": true, "redirect": "/login"})
		} else {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
		return
	case "/revoke-session":
		id := r.FormValue("session")
		if err := a.revokeSession(id); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
		if id == sessionID(cookie.Value) {
			http.SetCookie(w, &http.Cookie{Name: "roi_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.config.SecureCookies, SameSite: http.SameSiteStrictMode})
			if wantsAdminJSON(r) {
				_ = jsonReply(w, map[string]any{"ok": true, "redirect": "/login"})
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
	case "/rename":
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if err = a.store.RenameFeed(id, r.FormValue("title")); err != nil {
			a.adminStoreError(w, r, err)
			return
		}
	case "/revoke-api-tokens":
		e = a.RotateAPIKey()
		if e != nil {
			a.requestError(w, r, e)
			return
		}
	case "/logout":
		if err := a.revokeSession(sessionID(cookie.Value)); err != nil {
			a.requestError(w, r, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "roi_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.config.SecureCookies, SameSite: http.SameSiteStrictMode})
		if wantsAdminJSON(r) {
			_ = jsonReply(w, map[string]any{"ok": true, "redirect": "/login"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case "/import-roi":
		f, _, err := r.FormFile("roi")
		if err != nil {
			http.Error(w, "choose an ROI file", http.StatusBadRequest)
			return
		}
		defer f.Close()
		if err = a.store.ImportROI(f); err != nil {
			http.Error(w, "invalid ROI: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.poller.Wake()
	case "/import":
		f, _, err := r.FormFile("opml")
		if err != nil {
			http.Error(w, "choose an OPML file", http.StatusBadRequest)
			return
		}
		defer f.Close()
		if err = a.store.Import(f); err != nil {
			http.Error(w, "invalid OPML: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.poller.Wake()
	case "/sync":
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil || id < 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if id == 0 {
			query := "UPDATE feeds SET next_fetch=0 WHERE active=1 AND paused=0"
			switch r.FormValue("kind") {
			case "rss":
				query += " AND git_provider=''"
			case "git":
				query += " AND git_provider<>''"
			case "":
			default:
				http.Error(w, "invalid feed kind", http.StatusBadRequest)
				return
			}
			_, e = a.store.DB.Exec(query)
		} else {
			_, e = a.store.DB.Exec("UPDATE feeds SET next_fetch=0 WHERE id=? AND active=1 AND paused=0", id)
		}
		a.poller.Wake()
	case "/disable", "/enable", "/delete":
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/delete" {
			e = a.store.DeleteFeed(id)
		} else {
			e = a.store.SetEnabled(id, r.URL.Path == "/enable")
		}
		if e != nil {
			a.adminStoreError(w, r, e)
			return
		}
		if r.URL.Path != "/enable" {
			a.poller.Cancel(id)
		}
		a.poller.Wake()
	case "/toggle":
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		_, e = a.store.DB.Exec("UPDATE feeds SET paused=1-paused,next_fetch=0 WHERE id=? AND active=1", id)
		a.poller.Wake()
	default:
		http.NotFound(w, r)
		return
	}
	if e != nil {
		a.requestError(w, r, e)
		return
	}
	if wantsAdminJSON(r) {
		_ = jsonReply(w, map[string]any{"ok": true, "message": adminActionMessage(r.URL.Path)})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
