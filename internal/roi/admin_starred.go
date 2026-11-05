package roi

import (
	"net/http"
	"strconv"
)

// Include disabled feeds too: hiding a subscription does not discard saved articles.
func (a *App) starredPanel(r *http.Request, panel *starredPanel) error {
	if err := a.store.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM entries WHERE starred=1").Scan(&panel.Total); err != nil {
		return err
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("starred_page"))
	pages := max((panel.Total+49)/50, 1)
	panel.Page = min(max(page, 1), pages)
	link := func(page int) string {
		q := r.URL.Query()
		q.Del("fragment")
		q.Set("starred_page", strconv.Itoa(page))
		return "/starred?" + q.Encode() + "#starred-articles"
	}
	if panel.Page > 1 {
		panel.PreviousURL = link(panel.Page - 1)
	}
	if panel.Page < pages {
		panel.NextURL = link(panel.Page + 1)
	}
	rows, err := a.store.DB.QueryContext(r.Context(), `SELECT e.title,e.url,f.title,e.published,f.active FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE e.starred=1 ORDER BY e.id DESC LIMIT 50 OFFSET ?`, (panel.Page-1)*50)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var article starredArticle
		if err := rows.Scan(&article.Title, &article.URL, &article.Feed, &article.Published, &article.Active); err != nil {
			return err
		}
		panel.Articles = append(panel.Articles, article)
	}
	return rows.Err()
}
