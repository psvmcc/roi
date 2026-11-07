package roi

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Feed struct {
	Paused                                 bool
	HostQueued                             bool
	HostReason                             string
	HostResume                             int64
	ID                                     int64
	URL, Title, Site                       string
	UserAgent                              string
	SiteOverride                           string
	IconOverride                           string
	AutomaticIcon                          string
	AutomaticSite                          string
	Active                                 bool
	Next, Attempt, Success, New, Published int64
	Error                                  string
	Failures, Count, Unread, Bytes         int64
	VisibleUnread                          int64
	Folders                                []string
	GitProvider, GitRepo, GitMode, Icon    string
	GitTagFilter                           string
	GitPreview                             bool
	GitRevision                            int64
}

func (s *Store) Feeds() ([]Feed, error) {
	rows, e := s.DB.Query(`SELECT f.id,f.url,f.title,f.site,f.active,f.paused,f.next_fetch,f.last_attempt,f.last_success,f.last_new,COALESCE((SELECT MAX(published) FROM entries WHERE feed_id=f.id),0),f.error,f.failures,f.git_provider,f.git_repo,f.git_mode,f.git_preview,f.git_revision,CASE WHEN f.icon_override<>'' THEN f.icon_override ELSE f.icon_url END,f.user_agent,f.git_tag_filter,COALESCE(s.count,0),COALESCE(s.unread,0),COALESCE(s.bytes,0),COALESCE(s.visible_unread,0),f.site_override,f.icon_override,f.icon_url,f.site_auto FROM feeds f LEFT JOIN feed_stats s ON s.feed_id=f.id ORDER BY f.title`)
	if e != nil {
		return nil, e
	}
	fs := []Feed{}
	for rows.Next() {
		var f Feed
		if e = rows.Scan(&f.ID, &f.URL, &f.Title, &f.Site, &f.Active, &f.Paused, &f.Next, &f.Attempt, &f.Success, &f.New, &f.Published, &f.Error, &f.Failures, &f.GitProvider, &f.GitRepo, &f.GitMode, &f.GitPreview, &f.GitRevision, &f.Icon, &f.UserAgent, &f.GitTagFilter, &f.Count, &f.Unread, &f.Bytes, &f.VisibleUnread, &f.SiteOverride, &f.IconOverride, &f.AutomaticIcon, &f.AutomaticSite); e != nil {
			rows.Close()
			return nil, e
		}
		fs = append(fs, f)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	folders, e := s.DB.Query("SELECT feed_id,name FROM feed_folders ORDER BY feed_id,name")
	if e != nil {
		return nil, e
	}
	defer folders.Close()
	positions := map[int64]int{}
	for i, f := range fs {
		positions[f.ID] = i
	}
	for folders.Next() {
		var id int64
		var name string
		if e = folders.Scan(&id, &name); e != nil {
			return nil, e
		}
		if i, ok := positions[id]; ok {
			fs[i].Folders = append(fs[i].Folders, name)
		}
	}
	if e = folders.Err(); e != nil {
		return nil, e
	}
	return fs, nil
}

func feedID(s string) (int64, error) {
	if !strings.HasPrefix(s, "feed/") {
		return 0, fmt.Errorf("invalid feed id")
	}
	id, e := strconv.ParseInt(strings.TrimPrefix(s, "feed/"), 10, 64)
	if e != nil || id < 1 {
		return 0, fmt.Errorf("invalid feed id")
	}
	return id, nil
}

var (
	ErrFeedNotFound = fmt.Errorf("subscription not found")
	ErrFeedEnabled  = fmt.Errorf("disable the subscription before deleting it")
	ErrFeedDisabled = fmt.Errorf("subscription is disabled")
)

// SetEnabled preserves articles and folders. Enabling also resumes polling.
func (s *Store) SetEnabled(id int64, enabled bool) error {
	res, e := s.DB.Exec("UPDATE feeds SET active=?,paused=0,next_fetch=0 WHERE id=?", enabled, id)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return ErrFeedNotFound
	}
	return nil
}

// DeleteFeed is deliberately available only to the admin HTTP handler. The
// disabled check and all dependent deletions share one transaction.
func (s *Store) DeleteFeed(id int64) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var active bool
	if e = tx.QueryRow("SELECT active FROM feeds WHERE id=?", id).Scan(&active); e == sql.ErrNoRows {
		return ErrFeedNotFound
	} else if e != nil {
		return e
	}
	if active {
		return ErrFeedEnabled
	}
	for _, table := range []string{"fetches", "tombstones", "entries", "feed_folders"} {
		if _, e = tx.Exec("DELETE FROM "+table+" WHERE feed_id=?", id); e != nil {
			return e
		}
	}
	if _, e = tx.Exec("DELETE FROM feeds WHERE id=?", id); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) RenameFeed(id int64, title string) error {
	title = strings.TrimSpace(title)
	if !utf8.ValidString(title) || utf8.RuneCountInString(title) < 1 || utf8.RuneCountInString(title) > 512 || strings.ContainsFunc(title, unicode.IsControl) {
		return clientError("name must contain 1–512 characters without control characters")
	}
	res, e := s.DB.Exec("UPDATE feeds SET title=? WHERE id=?", title, id)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return ErrFeedNotFound
	}
	return nil
}

func (s *Store) SaveGitSource(id int64, g GitSource, iconOverride ...string) error {
	var override string
	if len(iconOverride) > 0 {
		override = strings.TrimSpace(iconOverride[0])
		if err := validateIconOverride(override); err != nil {
			return err
		}
	}
	if g.Provider == "auto" {
		return clientError("detect the Git service before saving")
	}
	if err := validateTagFilter(g.TagFilter); err != nil {
		return err
	}
	checked, err := parseGitSource(g.URL, g.Provider, g.Mode, g.Preview)
	if err != nil {
		return err
	}
	g.URL = checked.URL
	g.Repo = checked.Repo
	g.Mode = checked.Mode
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if id == 0 {
		var exists int
		if err = tx.QueryRow("SELECT count(*) FROM feeds WHERE url=?", g.URL).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return clientError("repository is already subscribed; edit the existing source")
		}
		result, err := tx.Exec("INSERT INTO feeds(url,title,site,git_provider,git_repo,git_mode,git_preview,icon_url,git_tag_filter,icon_override) VALUES(?,?,?,?,?,?,?,?,?,?)", g.URL, g.title(), g.URL, g.Provider, g.Repo, g.Mode, g.Preview, g.Icon, g.TagFilter, override)
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
	} else {
		result, err := tx.Exec("UPDATE feeds SET git_mode=?,git_preview=?,git_tag_filter=?,icon_override=CASE WHEN ? THEN ? ELSE icon_override END,git_revision=git_revision+1,next_fetch=0,error='',failures=0 WHERE id=? AND url=? AND git_provider=?", g.Mode, g.Preview, g.TagFilter, len(iconOverride) > 0, override, id, g.URL, g.Provider)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return clientError("unknown Git source or repository changed")
		}
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO folders(name) VALUES(?)", gitFolder); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO feed_folders(feed_id,name) VALUES(?,?)", id, gitFolder); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetFeedUserAgent(id int64, value string) error {
	return s.SetRSSSettings(id, value, nil, nil)
}

// SetRSSSettings changes the source in place. Nil URL fields leave existing settings intact.
func (s *Store) SetRSSSettings(id int64, agent string, raw, website *string, iconOverride ...string) error {
	var icon string
	if len(iconOverride) > 0 {
		icon = strings.TrimSpace(iconOverride[0])
		if err := validateIconOverride(icon); err != nil {
			return err
		}
	}
	if err := validateUserAgent(agent); err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentURL, site, override, automaticSite string
	if err = tx.QueryRow("SELECT url,site,site_override,site_auto FROM feeds WHERE id=? AND git_provider=''", id).Scan(&currentURL, &site, &override, &automaticSite); err == sql.ErrNoRows {
		return clientError("RSS subscription not found")
	} else if err != nil {
		return err
	}
	if raw != nil {
		value := strings.TrimSpace(*raw)
		if len(value) > 8192 {
			return clientError("RSS URL must not exceed 8192 bytes")
		}
		if err := validFeedURL(value); err != nil {
			return clientError(err.Error())
		}
		duplicate, err := importURLExistsExcept(tx, value, id)
		if err != nil {
			return err
		}
		if duplicate {
			return clientError("a subscription with this RSS URL already exists (HTTP and HTTPS variants count as duplicates)")
		}
		currentURL = value
	}
	if website != nil {
		value := strings.TrimSpace(*website)
		if len(value) > 8192 {
			return clientError("website URL must not exceed 8192 bytes")
		}
		if value != "" {
			if err := validFeedURL(value); err != nil {
				return clientError(err.Error())
			}
		}
		if value != "" {
			site = value
		} else if override != "" {
			site = automaticSite
		}
		override = value
	}
	_, err = tx.Exec("UPDATE feeds SET url=?,site=?,site_override=?,user_agent=?,icon_override=CASE WHEN ? THEN ? ELSE icon_override END,etag='',modified='',next_fetch=0,error='',failures=0,git_revision=git_revision+1 WHERE id=?", currentURL, site, override, strings.TrimSpace(agent), len(iconOverride) > 0, icon, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
