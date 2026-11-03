package roi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const userID = "1"
const statePrefix = "user/-/state/com.google/"

func sign(key, value string) string {
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}
func randomToken() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func (a *App) apiKey() string {
	a.mu.Lock()
	key := a.signingKey
	a.mu.Unlock()
	return sign(key, "roi-api-v2\n"+a.config.APIAuth.Username+"\n"+a.config.APIAuth.PasswordHash)
}
func (a *App) issueToken() string {
	a.keyRotation.Lock()
	defer a.keyRotation.Unlock()
	if a.store == nil {
		return ""
	}
	v := "v2.0." + randomToken()
	token := v + "." + sign(a.apiKey(), v)
	if err := a.createSession(token, "api", time.Time{}, nil); err != nil {
		return ""
	}
	return token
}
func (a *App) tokenSignatureValid(token string) bool {
	v := strings.Split(token, ".")
	if len(v) != 4 || v[0] != "v2" || len(v[2]) != 64 || len(v[3]) != 64 {
		return false
	}
	exp, e := strconv.ParseInt(v[1], 10, 64)
	if _, err := hex.DecodeString(v[2]); err != nil {
		return false
	}
	// Registered legacy tokens retain their original signed timestamp, while the
	// database determines session lifetime. Active legacy sessions are upgraded
	// to no-expiry on startup; revoked/expired records are never revived.
	return e == nil && exp >= 0 && hmac.Equal([]byte(sign(a.apiKey(), strings.Join(v[:3], "."))), []byte(v[3]))
}
func (a *App) validToken(token string) bool {
	return a.tokenSignatureValid(token) && a.sessionActive(sessionID(token))
}
func jsonReply(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(v)
}
func (a *App) api(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, a.config.API.Prefix)
	if path == "/accounts/ClientLogin" {
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		valid, err := checkCredentialsContext(r.Context(), a.config.APIAuth, r.PostForm.Get("Email"), r.PostForm.Get("Passwd"))
		if err != nil {
			http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if !valid {
			http.Error(w, "Error=BadAuthentication", http.StatusForbidden)
			return
		}
		token := a.issueToken()
		if token == "" {
			http.Error(w, "session storage unavailable", http.StatusServiceUnavailable)
			return
		}
		a.touchSession(r, sessionID(token))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "SID=%s\nLSID=%s\nAuth=%s\n", token, token, token)
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "GoogleLogin auth=")
	if !a.validToken(auth) {
		if a.tokenSignatureValid(auth) {
			if err := a.store.DB.PingContext(r.Context()); err != nil {
				a.requestError(w, r, err)
				return
			}
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	a.touchSession(r, sessionID(auth))
	select {
	case a.apiSlots <- struct{}{}:
		defer func() { <-a.apiSlots }()
	case <-r.Context().Done():
		return
	case <-time.After(5 * time.Second):
		rateLimited(w)
		return
	}
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if e := r.ParseForm(); e != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	for _, key := range []string{"i", "s", "a", "r", "it", "xt"} {
		if v, ok := r.Form[key+"[]"]; ok {
			r.Form[key] = append(r.Form[key], v...)
			delete(r.Form, key+"[]")
		}
	}
	route := strings.TrimPrefix(path, "/reader/api/0/")
	mutating := map[string]bool{"edit-tag": true, "mark-all-as-read": true, "subscription/edit": true, "subscription/quickadd": true, "rename-tag": true, "disable-tag": true}
	if mutating[route] {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if route != "subscription/quickadd" && route != "subscription/edit" && !a.validActionToken(r.FormValue("T"), auth) {
			http.Error(w, "invalid token", http.StatusForbidden)
			return
		}
	}
	approved, hidden, err := a.readerAccess(sessionID(auth))
	if err != nil {
		a.requestError(w, r, err)
		return
	}
	if (!approved || hidden) && route != "token" && route != "user-info" && route != "preference/list" && route != "preference/stream/list" {
		if mutating[route] {
			if hidden {
				http.Error(w, "feeds are hidden for this device", http.StatusForbidden)
				return
			}
			http.Error(w, "device approval required", http.StatusForbidden)
			return
		}
		if err := pendingAPI(w, r, route); err != nil {
			a.requestError(w, r, err)
		}
		return
	}
	if e := a.apiRoute(w, r, route, auth); e != nil {
		a.requestError(w, r, e)
	}
}
func checkCredentials(c Credentials, name, password string) bool {
	ok := VerifyPassword(c.PasswordHash, []byte(password))
	return hmac.Equal([]byte(name), []byte(c.Username)) && ok
}
func checkCredentialsContext(ctx context.Context, c Credentials, name, password string) (bool, error) {
	ok, err := VerifyPasswordContext(ctx, c.PasswordHash, []byte(password))
	return hmac.Equal([]byte(name), []byte(c.Username)) && ok, err
}
func (a *App) apiRoute(w http.ResponseWriter, r *http.Request, route, auth string) error {
	switch route {
	case "token":
		w.Header().Set("Content-Type", "text/plain")
		_, e := fmt.Fprintln(w, a.actionToken(auth))
		return e
	case "preference/list":
		return jsonReply(w, map[string]any{"prefs": []any{}})
	case "preference/stream/list":
		return jsonReply(w, map[string]any{"streamprefs": map[string]any{}})
	case "user-info":
		return jsonReply(w, map[string]any{"userId": userID, "userName": a.config.APIAuth.Username, "userProfileId": userID, "userEmail": a.config.APIAuth.Username})
	case "subscription/list":
		fs, e := a.store.Feeds()
		if e != nil {
			return e
		}
		subs := []any{}
		for _, f := range fs {
			if !f.Active {
				continue
			}
			cats := []any{}
			for _, g := range f.Folders {
				cats = append(cats, map[string]string{"id": "user/1/label/" + g, "label": g})
			}
			subs = append(subs, map[string]any{"id": fmt.Sprintf("feed/%d", f.ID), "title": f.Title, "url": f.URL, "htmlUrl": f.Site, "categories": cats, "sortid": fmt.Sprintf("%08x", f.ID), "firstitemmsec": "0", "iconUrl": f.Icon})
		}
		return jsonReply(w, map[string]any{"subscriptions": subs})
	case "tag/list":
		tags := []any{map[string]string{"id": statePrefix + "starred", "type": "state"}, map[string]string{"id": statePrefix + "reading-list", "type": "state"}}
		rows, e := a.store.DB.Query("SELECT name FROM folders WHERE EXISTS (SELECT 1 FROM feed_folders ff JOIN feeds f ON f.id=ff.feed_id WHERE ff.name=folders.name AND f.active=1) ORDER BY name")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if e = rows.Scan(&n); e != nil {
				return e
			}
			tags = append(tags, map[string]string{"id": "user/1/label/" + n, "type": "folder"})
		}
		if e = rows.Err(); e != nil {
			return e
		}
		return jsonReply(w, map[string]any{"tags": tags})
	case "unread-count":
		fs, e := a.store.Feeds()
		if e != nil {
			return e
		}
		counts := []any{}
		total := int64(0)
		groups := map[string]int64{}
		for _, f := range fs {
			if !f.Active {
				continue
			}
			f.Unread = f.VisibleUnread
			total += f.Unread
			counts = append(counts, map[string]any{"id": fmt.Sprintf("feed/%d", f.ID), "count": f.Unread, "newestItemTimestampUsec": strconv.FormatInt(f.New*1e6, 10)})
			for _, g := range f.Folders {
				groups[g] += f.Unread
			}
		}
		for g, n := range groups {
			counts = append(counts, map[string]any{"id": "user/1/label/" + g, "count": n, "newestItemTimestampUsec": "0"})
		}
		counts = append(counts, map[string]any{"id": "user/1/state/com.google/reading-list", "count": total, "newestItemTimestampUsec": strconv.FormatInt(time.Now().UnixMicro(), 10)})
		return jsonReply(w, map[string]any{"max": 2147483647, "unreadcounts": counts})
	case "stream/items/ids":
		return a.stream(w, r, true, "")
	case "stream/items/contents":
		return a.contents(w, r)
	case "edit-tag":
		return a.editTag(w, r)
	case "mark-all-as-read":
		return a.markRead(w, r)
	case "subscription/quickadd", "subscription/edit":
		return a.subscription(w, r, route)
	case "rename-tag", "disable-tag":
		return a.folder(w, r, route)
	default:
		if strings.HasPrefix(route, "stream/contents/") {
			return a.stream(w, r, false, strings.TrimPrefix(route, "stream/contents/"))
		}
		http.NotFound(w, r)
		return nil
	}
}

// A typed client error never exposes SQL or internals to the caller.
type clientError string

func (e clientError) Error() string { return string(e) }
func selector(v string) (string, []any, error) {
	switch v {
	case "", "reading-list", statePrefix + "reading-list", "user/1/state/com.google/reading-list":
		return "f.active=1", nil, nil
	case statePrefix + "read", "user/1/state/com.google/read":
		return "e.is_read=1", nil, nil
	case statePrefix + "starred", "user/1/state/com.google/starred":
		return "e.starred=1", nil, nil
	case statePrefix + "kept-unread", "user/1/state/com.google/kept-unread", statePrefix + "unread", "user/1/state/com.google/unread":
		return "e.is_read=0", nil, nil
	}
	if strings.HasPrefix(v, "feed/") {
		id, e := feedID(v)
		if e != nil {
			return "", nil, clientError("invalid feed")
		}
		return "e.feed_id=?", []any{id}, nil
	}
	if i := strings.Index(v, "/label/"); i >= 0 {
		return "EXISTS(SELECT 1 FROM feed_folders ff WHERE ff.feed_id=e.feed_id AND ff.name=?)", []any{v[i+7:]}, nil
	}
	return "", nil, clientError("unsupported stream")
}
func filter(v url.Values) (string, []any, error) {
	parts := []string{readerEntryFilter}
	args := []any{}
	streams := v["s"]
	if len(streams) == 0 {
		streams = []string{""}
	}
	ors := []string{}
	for _, s := range streams {
		p, a, e := selector(s)
		if e != nil {
			return "", nil, e
		}
		ors = append(ors, "("+p+")")
		args = append(args, a...)
	}
	parts = append(parts, "("+strings.Join(ors, " OR ")+")")
	for _, x := range v["xt"] {
		p, a, e := selector(x)
		if e != nil {
			return "", nil, e
		}
		parts = append(parts, "NOT ("+p+")")
		args = append(args, a...)
	}
	for _, x := range v["it"] {
		p, a, e := selector(x)
		if e != nil {
			return "", nil, e
		}
		parts = append(parts, "("+p+")")
		args = append(args, a...)
	}
	for _, k := range []string{"ot", "nt"} {
		if v.Get(k) != "" {
			n, e := strconv.ParseInt(v.Get(k), 10, 64)
			if e != nil || n < 0 {
				return "", nil, clientError("invalid timestamp")
			}
			op := ">="
			if k == "nt" {
				op = "<="
			}
			parts = append(parts, "e.updated"+op+"?")
			args = append(args, n)
		}
	}
	return strings.Join(parts, " AND "), args, nil
}

type Entry struct {
	ID, FeedID                                   int64
	Title, URL, Content, Author, FeedTitle, Site string
	Published, Received, Updated                 int64
	Read, Starred                                bool
}

const entryColumns = "e.id,e.feed_id,e.title,e.url,e.content,e.author,f.title,f.site,e.published,e.received,e.updated,e.is_read,e.starred"

func entryID(id int64) string { return fmt.Sprintf("tag:google.com,2005:reader/item/%016x", id) }
func parseEntryID(v string) (int64, error) {
	base := 10
	if i := strings.LastIndex(v, "/item/"); i >= 0 {
		v = v[i+6:]
		base = 16
	} else if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
		v = v[2:]
		base = 16
	} else if strings.HasPrefix(v, "0") || strings.ContainsAny(v, "abcdefABCDEF") {
		base = 16
	}
	// Reeder requests article contents using bare, zero-padded hexadecimal IDs.
	// Decimal itemRefs, bare hex, and long Google Reader IDs address the same row.
	id, e := strconv.ParseInt(v, base, 64)
	if e != nil || id < 1 {
		return 0, clientError("invalid item id")
	}
	return id, nil
}

func (e Entry) wire() map[string]any {
	cats := []string{"user/1/state/com.google/reading-list"}
	if e.Read {
		cats = append(cats, "user/1/state/com.google/read")
	} else {
		cats = append(cats, "user/1/state/com.google/kept-unread")
	}
	if e.Starred {
		cats = append(cats, "user/1/state/com.google/starred")
	}
	return map[string]any{"id": entryID(e.ID), "title": e.Title, "published": e.Published, "updated": e.Updated, "crawlTimeMsec": strconv.FormatInt(e.Received/1000, 10), "timestampUsec": strconv.FormatInt(e.Published*1e6, 10), "categories": cats, "author": e.Author, "alternate": []any{map[string]string{"href": e.URL, "type": "text/html"}}, "canonical": []any{map[string]string{"href": e.URL}}, "summary": map[string]string{"content": e.Content, "direction": "ltr"}, "origin": map[string]string{"streamId": fmt.Sprintf("feed/%d", e.FeedID), "title": e.FeedTitle, "htmlUrl": e.Site}}
}

type cursor struct {
	Max, Last int64
	Asc       bool
	Query     string
}

func (a *App) encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	v := base64.RawURLEncoding.EncodeToString(b)
	return v + "." + sign(a.apiKey(), "cursor:"+v)
}
func (a *App) decodeCursor(v string) (cursor, error) {
	var c cursor
	p := strings.Split(v, ".")
	if len(p) != 2 || !hmac.Equal([]byte(p[1]), []byte(sign(a.apiKey(), "cursor:"+p[0]))) {
		return c, clientError("invalid continuation")
	}
	b, e := base64.RawURLEncoding.DecodeString(p[0])
	if e != nil {
		return c, clientError("invalid continuation")
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, clientError("invalid continuation")
	}
	return c, nil
}
func (a *App) stream(w http.ResponseWriter, r *http.Request, idsOnly bool, pathStream string) error {
	v := cloneValues(r.Form)
	if pathStream != "" {
		v.Set("s", pathStream)
	}
	where, args, e := filter(v)
	if e != nil {
		return e
	}
	asc := v.Get("r") == "o"
	// Only parameters that affect membership/order belong to a continuation.
	// Clients change cache busters such as ck on each page.
	fingerprint := url.Values{}
	for _, key := range []string{"s", "xt", "it", "ot", "nt", "r"} {
		if values, ok := v[key]; ok {
			fingerprint[key] = append([]string(nil), values...)
			sort.Strings(fingerprint[key])
		}
	}
	query := sign(a.apiKey(), fingerprint.Encode())
	c := cursor{Asc: asc, Query: query}
	if v.Get("c") != "" {
		c, e = a.decodeCursor(v.Get("c"))
		if e != nil {
			return e
		}
		if c.Asc != asc || c.Query != query {
			return clientError("continuation does not match query")
		}
	} else {
		if e = a.store.DB.QueryRow("SELECT COALESCE(MAX(id),0) FROM entries").Scan(&c.Max); e != nil {
			return e
		}
	}
	where += " AND e.id<=?"
	args = append(args, c.Max)
	order := "DESC"
	if asc {
		order = "ASC"
	}
	if c.Last > 0 {
		op := "<"
		if asc {
			op = ">"
		}
		where += " AND e.id" + op + "?"
		args = append(args, c.Last)
	}
	n := 1000
	if v.Get("n") != "" {
		n, e = strconv.Atoi(v.Get("n"))
		if e != nil || n < 1 {
			return clientError("invalid n")
		}
		n = min(n, 10000)
	}
	args = append(args, n+1)
	if !idsOnly {
		return a.streamArticleContents(w, r, where, args, order, v.Get("s"), c, n)
	}
	rows, e := a.store.DB.Query("SELECT e.id,e.feed_id,e.received,e.published FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE "+where+" ORDER BY e.id "+order+" LIMIT ?", args...)
	if e != nil {
		return e
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var x Entry
		if e = rows.Scan(&x.ID, &x.FeedID, &x.Received, &x.Published); e != nil {
			return e
		}
		entries = append(entries, x)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	continuation := ""
	if len(entries) > n {
		entries = entries[:n]
		c.Last = entries[len(entries)-1].ID
		continuation = a.encodeCursor(c)
	}
	list := []any{}
	for _, x := range entries {
		list = append(list, map[string]any{"id": strconv.FormatInt(x.ID, 10), "directStreamIds": []string{fmt.Sprintf("feed/%d", x.FeedID)}, "timestampUsec": strconv.FormatInt(x.Published*1e6, 10)})
	}
	result := map[string]any{"itemRefs": list}
	if continuation != "" {
		result["continuation"] = continuation
	}
	return jsonReply(w, result)
}
func (a *App) contents(w http.ResponseWriter, r *http.Request) error {
	values := r.Form["i"]
	if len(values) > 10000 {
		return clientError("too many ids")
	}
	ids := make([]int64, 0, len(values))
	for _, raw := range values {
		id, e := parseEntryID(raw)
		if e != nil {
			return e
		}
		ids = append(ids, id)
	}
	return a.articleContents(w, r, ids)
}
func (a *App) editTag(w http.ResponseWriter, r *http.Request) error {
	tx, e := a.store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	ids := r.Form["i"]
	if len(ids) > 10000 {
		return clientError("too many ids")
	}
	for _, v := range ids {
		id, e := parseEntryID(v)
		if e != nil {
			return e
		}
		for _, op := range []string{"a", "r"} {
			for _, tag := range r.Form[op] {
				col := ""
				value := 0
				if op == "a" {
					value = 1
				}
				switch {
				case strings.HasSuffix(tag, "/state/com.google/read"):
					col = "is_read"
				case strings.HasSuffix(tag, "/state/com.google/starred"):
					col = "starred"
				case strings.HasSuffix(tag, "/state/com.google/kept-unread"):
					col = "is_read"
					value = 1 - value
				default:
					return clientError("unsupported tag")
				}
				if _, e = tx.Exec("UPDATE entries SET "+col+"=? WHERE id IN (SELECT e.id FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE e.id=? AND "+readerEntryFilter+")", value, id); e != nil {
					return e
				}
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	_, e = fmt.Fprint(w, "OK")
	return e
}
func (a *App) markRead(w http.ResponseWriter, r *http.Request) error {
	where, args, e := filter(r.Form)
	if e != nil {
		return e
	}
	ts := time.Now().UnixMicro()
	if r.FormValue("ts") != "" {
		ts, e = strconv.ParseInt(r.FormValue("ts"), 10, 64)
		if e != nil || ts < 0 {
			return clientError("invalid ts")
		}
	}
	where += " AND e.received<=?"
	args = append(args, ts)
	_, e = a.store.DB.Exec("UPDATE entries SET is_read=1 WHERE id IN (SELECT e.id FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE "+where+")", args...)
	if e != nil {
		return e
	}
	_, e = fmt.Fprint(w, "OK")
	return e
}
func (a *App) subscription(w http.ResponseWriter, r *http.Request, route string) error {
	for _, op := range []string{"a", "r"} {
		for _, tag := range r.Form[op] {
			if i := strings.Index(tag, "/label/"); i >= 0 && reservedGitFolder(tag[i+7:]) {
				return &statusError{http.StatusForbidden, "GIT_Releases is managed only in the admin"}
			}
		}
	}

	tx, e := a.store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	action := r.FormValue("ac")
	raw := r.FormValue("s")
	var id int64
	if route == "subscription/quickadd" || action == "subscribe" {
		if route == "subscription/quickadd" {
			raw = r.FormValue("quickadd")
		}
		raw = strings.TrimPrefix(raw, "feed/")
		if e = validFeedURL(raw); e != nil {
			return clientError(e.Error())
		}
		var provider string
		err := tx.QueryRow("SELECT git_provider FROM feeds WHERE url=?", raw).Scan(&provider)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if provider != "" {
			return &statusError{http.StatusForbidden, "Git sources can be added or enabled only in the admin"}
		}
		title := r.FormValue("t")
		if title == "" {
			title = raw
		}
		if _, e = tx.Exec("INSERT INTO feeds(url,title) VALUES(?,?) ON CONFLICT(url) DO UPDATE SET active=1,paused=0,next_fetch=0", raw, title); e != nil {
			return e
		}
		if e = tx.QueryRow("SELECT id FROM feeds WHERE url=?", raw).Scan(&id); e != nil {
			return e
		}
	} else {
		id, e = feedID(raw)
		if e != nil {
			return clientError("invalid feed")
		}
		var exists int
		if e = tx.QueryRow("SELECT 1 FROM feeds WHERE id=?", id).Scan(&exists); e != nil {
			return clientError("unknown feed")
		}
		switch action {
		case "unsubscribe":
			_, e = tx.Exec("UPDATE feeds SET active=0,paused=0 WHERE id=?", id)
		case "edit":
			var active bool
			if err := tx.QueryRow("SELECT active FROM feeds WHERE id=?", id).Scan(&active); err != nil {
				return err
			}
			if !active {
				return clientError("subscription is disabled")
			}
			if t := r.FormValue("t"); t != "" {
				_, e = tx.Exec("UPDATE feeds SET title=? WHERE id=?", t, id)
			}
		default:
			return clientError("unsupported action")
		}
		if e != nil {
			return e
		}
	}
	for _, op := range []string{"a", "r"} {
		for _, tag := range r.Form[op] {
			i := strings.Index(tag, "/label/")
			if i < 0 {
				return clientError("invalid folder")
			}
			name := tag[i+7:]
			if op == "a" {
				if _, e = tx.Exec("INSERT OR IGNORE INTO folders VALUES(?)", name); e == nil {
					_, e = tx.Exec("INSERT OR IGNORE INTO feed_folders VALUES(?,?)", id, name)
				}
			} else {
				_, e = tx.Exec("DELETE FROM feed_folders WHERE feed_id=? AND name=?", id, name)
			}
			if e != nil {
				return e
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if action == "unsubscribe" {
		a.poller.Cancel(id)
	}
	a.poller.Wake()
	if route == "subscription/quickadd" {
		return jsonReply(w, map[string]any{"numResults": 1, "query": raw, "streamId": fmt.Sprintf("feed/%d", id)})
	}
	_, e = fmt.Fprint(w, "OK")
	return e
}
func (a *App) folder(w http.ResponseWriter, r *http.Request, route string) error {
	name := r.FormValue("s")
	if route == "rename-tag" {
		name = r.FormValue("s")
	}
	i := strings.Index(name, "/label/")
	if i < 0 {
		return clientError("invalid folder")
	}
	name = name[i+7:]
	target := r.FormValue("dest")
	if i := strings.Index(target, "/label/"); i >= 0 {
		target = target[i+7:]
	}
	if reservedGitFolder(name) || reservedGitFolder(target) {
		return &statusError{http.StatusForbidden, "GIT_Releases cannot be changed through the API"}
	}
	tx, e := a.store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if route == "disable-tag" {
		_, e = tx.Exec("DELETE FROM folders WHERE name=?", name)
	} else {
		target := r.FormValue("dest")
		j := strings.Index(target, "/label/")
		if j < 0 {
			return clientError("invalid destination")
		}
		target = target[j+7:]
		if _, e = tx.Exec("INSERT OR IGNORE INTO folders VALUES(?)", target); e == nil {
			_, e = tx.Exec("INSERT OR IGNORE INTO feed_folders SELECT feed_id,? FROM feed_folders WHERE name=?", target, name)
		}
		if e == nil && target != name {
			_, e = tx.Exec("DELETE FROM folders WHERE name=?", name)
		}
	}
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	_, e = fmt.Fprint(w, "OK")
	return e
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, a := range v {
		out[k] = append([]string(nil), a...)
	}
	return out
}

func (a *App) actionToken(auth string) string { return sign(a.apiKey(), "csrf:"+auth)[:57] }
func (a *App) validActionToken(token, auth string) bool {
	// Reader clients authenticate every request with a bearer token, never an
	// admin cookie. Reeder sends T=x; some clients omit T altogether.
	return token == "" || token == "x" || hmac.Equal([]byte(token), []byte(a.actionToken(auth))) || hmac.Equal([]byte(token), []byte(sign(a.apiKey(), "csrf:"+auth)))
}

// Pending clients get normal, empty Reader responses so they can finish account
// setup without receiving any subscription, folder, item ID or article content.
func pendingAPI(w http.ResponseWriter, r *http.Request, route string) error {
	switch route {
	case "subscription/list":
		return jsonReply(w, map[string]any{"subscriptions": []any{}})
	case "tag/list":
		return jsonReply(w, map[string]any{"tags": []any{}})
	case "unread-count":
		return jsonReply(w, map[string]any{"max": 2147483647, "unreadcounts": []any{}})
	case "stream/items/ids":
		return jsonReply(w, map[string]any{"itemRefs": []any{}})
	case "stream/items/contents":
		return emptyReaderStream(w, "user/1/state/com.google/reading-list")
	default:
		if strings.HasPrefix(route, "stream/contents/") {
			return emptyReaderStream(w, strings.TrimPrefix(route, "stream/contents/"))
		}
		http.NotFound(w, r)
		return nil
	}
}
func emptyReaderStream(w http.ResponseWriter, id string) error {
	return jsonReply(w, map[string]any{"id": id, "updated": time.Now().Unix(), "direction": "ltr", "title": "roi", "items": []any{}})
}
