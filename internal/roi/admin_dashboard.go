package roi

import (
	"database/sql"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type fetchLog struct {
	Feed              string
	Started, Finished int64
	Status, Added     int
	Error             string
}

type sessionPanel struct {
	CSRF, ScrollID, Label string
	Sessions              []SessionInfo
	ShowExpiry            bool
	Page, Total           int
	PreviousURL, NextURL  string
}

type feedPanel struct {
	CSRF, ID, ScrollID, Label string
	Feeds                     []Feed
	DisabledOnly, IsGit       bool
	DisabledFeeds             int64
}

type dashboard struct {
	CSRF                                                         string
	Feeds                                                        []Feed
	RSSPanel, GitPanel                                           feedPanel
	Logs                                                         []fetchLog
	DataBytes, DBBytes, WALBytes, Entries, LastRotation, Deleted int64
	Unread, EnabledFeeds, DisabledFeeds, DisabledUnread          int64
	StoredUnread                                                 int64
	SpaceUsage                                                   SpaceUsage
	DisabledOnly                                                 bool
	ReaderSessions, BrowserSessions                              sessionPanel
	SecurityEvents                                               []SecurityEvent
}

func (a *App) adminDashboard(w http.ResponseWriter, r *http.Request, cookie *http.Cookie, csrf string) {
	var e error
	var err error
	d := dashboard{CSRF: csrf, DisabledOnly: r.URL.Query().Get("disabled") == "1"}
	d.RSSPanel = feedPanel{CSRF: csrf, ID: "feed-list", ScrollID: "feed-scroll", Label: "RSS Feeds", DisabledOnly: d.DisabledOnly}
	d.GitPanel = feedPanel{CSRF: csrf, ID: "git-feed-list", ScrollID: "git-feed-scroll", Label: "GIT Releases Feeds", DisabledOnly: d.DisabledOnly, IsGit: true}
	d.ReaderSessions = sessionPanel{CSRF: csrf, ScrollID: "reader-session-scroll", Label: "Reader API sessions"}
	d.BrowserSessions = sessionPanel{CSRF: csrf, ScrollID: "browser-session-scroll", Label: "Browser sessions", ShowExpiry: true}
	if err := a.sessionPanels(r, sessionID(cookie.Value), &d.ReaderSessions, &d.BrowserSessions); err != nil {
		a.requestError(w, r, err)
		return
	}
	d.SecurityEvents, err = a.securityHistory()
	if err != nil {
		a.requestError(w, r, err)
		return
	}
	allFeeds, err := a.store.Feeds()
	e = err
	if e != nil {
		a.requestError(w, r, e)
		return
	}
	for _, f := range allFeeds {
		if u, err := url.Parse(f.URL); err == nil && f.Active && !f.Paused {
			ready, busy := a.poller.hostStatus(hostGroup(u))
			f.HostQueued = busy && f.Next <= time.Now().Unix()
			if f.HostQueued || ready.After(time.Unix(f.Next, 0)) {
				f.HostReason, f.HostResume = a.poller.hostWaitReason(hostGroup(u))
			}
			if ready.After(time.Now()) {
				f.Next = max(f.Next, (ready.UnixMilli()+999)/1000)
			}
		}
		d.DataBytes += f.Bytes
		d.Entries += f.Count
		d.StoredUnread += f.Unread
		if f.Active {
			d.EnabledFeeds++
			d.Unread += f.VisibleUnread
			f.Unread = f.VisibleUnread
		} else {
			d.DisabledFeeds++
			if f.GitProvider == "" {
				d.RSSPanel.DisabledFeeds++
			} else {
				d.GitPanel.DisabledFeeds++
			}
			d.DisabledUnread += f.Unread
		}
		if f.Active != d.DisabledOnly {
			d.Feeds = append(d.Feeds, f)
		}
	}
	sort.SliceStable(d.Feeds, func(i, j int) bool {
		x, y := d.Feeds[i], d.Feeds[j]
		if x.Paused != y.Paused {
			return x.Paused
		}
		xe, ye := x.Error != "" || x.Failures > 0, y.Error != "" || y.Failures > 0
		if xe != ye {
			return xe
		}
		if x.Failures != y.Failures {
			return x.Failures > y.Failures
		}
		return strings.ToLower(x.Title) < strings.ToLower(y.Title)
	})
	for _, f := range d.Feeds {
		if f.GitProvider == "" {
			d.RSSPanel.Feeds = append(d.RSSPanel.Feeds, f)
		} else {
			d.GitPanel.Feeds = append(d.GitPanel.Feeds, f)
		}
	}
	if st, e := os.Stat(a.config.Database); e == nil {
		d.DBBytes = st.Size()
	}
	if st, e := os.Stat(a.config.Database + "-wal"); e == nil {
		d.WALBytes = st.Size()
	}
	rows, e := a.store.DB.Query("SELECT f.title,x.started,x.finished,x.status,x.added,x.error FROM fetches x JOIN feeds f ON f.id=x.feed_id ORDER BY x.id DESC LIMIT 50")
	if e != nil {
		a.requestError(w, r, e)
		return
	}
	for rows.Next() {
		var l fetchLog
		if e = rows.Scan(&l.Feed, &l.Started, &l.Finished, &l.Status, &l.Added, &l.Error); e != nil {
			break
		}
		d.Logs = append(d.Logs, l)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		a.requestError(w, r, e)
		return
	}
	e = a.store.DB.QueryRow("SELECT last_run,deleted FROM maintenance WHERE id=1").Scan(&d.LastRotation, &d.Deleted)
	if e != nil && e != sql.ErrNoRows {
		a.requestError(w, r, e)
		return
	}
	d.SpaceUsage, err = a.store.Space(r.Context())
	if err != nil {
		a.requestError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := "admin.html"
	if r.URL.Query().Get("fragment") == "1" {
		name = "dashboard-fragments"
	}
	if e = adminTemplate.ExecuteTemplate(w, name, d); e != nil {
		a.requestError(w, r, e)
	}
}
