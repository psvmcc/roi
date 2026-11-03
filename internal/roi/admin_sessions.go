package roi

import (
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (a *App) sessionPanels(r *http.Request, current string, reader, browser *sessionPanel) error {
	for _, entry := range []struct {
		realm, param string
		panel        *sessionPanel
	}{{"api", "reader_page", reader}, {"admin", "browser_page", browser}} {
		panel := entry.panel
		if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM auth_sessions WHERE realm=? AND revoked=0 AND (expires>? OR (realm='api' AND expires=0))", entry.realm, time.Now().Unix()).Scan(&panel.Total); err != nil {
			return err
		}
		page, _ := strconv.Atoi(r.URL.Query().Get(entry.param))
		pages := max((panel.Total+99)/100, 1)
		panel.Page = min(max(page, 1), pages)
		var err error
		panel.Sessions, err = a.querySessions(current, entry.realm, panel.Page)
		if err != nil {
			return err
		}
		link := func(n int) string {
			q := url.Values{}
			for _, key := range []string{"disabled", "reader_page", "browser_page"} {
				if value := r.URL.Query().Get(key); value != "" {
					q.Set(key, value)
				}
			}
			q.Set(entry.param, strconv.Itoa(n))
			return "/?" + q.Encode()
		}
		if panel.Page > 1 {
			panel.PreviousURL = link(panel.Page - 1)
		}
		if panel.Page < pages {
			panel.NextURL = link(panel.Page + 1)
		}
	}
	return nil
}
