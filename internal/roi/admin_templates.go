package roi

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/psvmcc/roi/internal/buildinfo"
	"github.com/psvmcc/roi/internal/web"
)

var adminTemplate = template.Must(template.New("admin.html").Funcs(template.FuncMap{"buildVersion": buildinfo.Label, "buildInfo": buildinfo.String, "userAgent": buildinfo.UserAgent, "when": adminTime, "shortSession": shortSession, "size": func(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 2, 64) + " MiB"
}}).ParseFS(web.Files, "templates/*.html"))

func (a *App) deleteConfirmation(w http.ResponseWriter, r *http.Request, csrf string) {
	id, e := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if e != nil || id < 1 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	fs, e := a.store.Feeds()
	if e != nil {
		a.requestError(w, r, e)
		return
	}
	for _, f := range fs {
		if f.ID != id {
			continue
		}
		if f.Active {
			a.adminStoreError(w, r, ErrFeedEnabled)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if e = adminTemplate.ExecuteTemplate(w, "delete.html", struct {
			CSRF string
			Feed Feed
		}{csrf, f}); e != nil {
			a.requestError(w, r, e)
		}
		return
	}
	a.adminStoreError(w, r, ErrFeedNotFound)
}

// The UTC fallback is usable without JavaScript. The browser formats these
// numeric timestamps in its own timezone on initial render and fragment refresh.
func adminTime(n int64) template.HTML {
	if n == 0 {
		return template.HTML("—")
	}
	date := time.Unix(n, 0).UTC()
	return template.HTML(fmt.Sprintf(`<time datetime="%s" data-unix="%d" title="UTC">%s</time>`, date.Format(time.RFC3339), n, date.Format("2006.01.02 15:04:05")))
}
