package roi

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mmcdole/gofeed"
	xhtml "golang.org/x/net/html"
)

// Gitweb traditionally separates query parameters with semicolons. Normalize
// them before using net/url, which intentionally rejects that legacy syntax.
func gitwebQuery(raw string) (url.Values, error) {
	return url.ParseQuery(strings.ReplaceAll(raw, ";", "&"))
}
func parseGitwebSource(u *url.URL, mode string, preview bool) (GitSource, error) {
	s := GitSource{Provider: "gitweb", Mode: "tags", Preview: preview}
	if mode != "tags" && mode != "releases" {
		return s, clientError("choose releases or tags")
	}
	q, err := gitwebQuery(u.RawQuery)
	if err != nil || len(q["p"]) != 1 {
		return s, clientError("Gitweb URL must include one p=repository.git parameter")
	}
	repo := q.Get("p")
	if len(repo) > 512 || !utf8.ValidString(repo) || strings.ContainsFunc(repo, unicode.IsControl) || strings.TrimSuffix(repo, ".git") == "" || !strings.HasSuffix(repo, ".git") {
		return s, clientError("Gitweb repository must end in .git")
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\?#%;\x00\r\n\t ") {
			return s, clientError("invalid Gitweb repository path")
		}
	}
	if u.RawPath != "" {
		return s, clientError("use a Gitweb URL without encoded path components")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = url.Values{"p": {repo}, "a": {"summary"}}.Encode()
	s.URL = u.String()
	s.Repo = strings.TrimSuffix(repo, ".git")
	s.Icon = u.Scheme + "://" + u.Host + "/favicon.ico"
	return s, nil
}
func (s GitSource) gitwebURL(action, ref string) string {
	u, _ := url.Parse(s.URL)
	q := url.Values{"p": {s.Repo + ".git"}, "a": {action}}
	if ref != "" {
		q.Set("h", ref)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
func htmlAttr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func htmlClass(n *xhtml.Node, class string) bool {
	for _, v := range strings.Fields(htmlAttr(n, "class")) {
		if v == class {
			return true
		}
	}
	return false
}
func htmlWalk(n *xhtml.Node, visit func(*xhtml.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		htmlWalk(c, visit)
	}
}
func htmlText(n *xhtml.Node) string {
	var b strings.Builder
	htmlWalk(n, func(c *xhtml.Node) {
		if c.Type == xhtml.TextNode {
			text := c.Data
			if c.PrevSibling != nil && c.PrevSibling.Type == xhtml.ElementNode && c.PrevSibling.Data == "br" {
				text = strings.TrimPrefix(text, "\n")
			}
			b.WriteString(text)
		} else if c.Type == xhtml.ElementNode && c.Data == "br" {
			b.WriteByte('\n')
		}
	})
	return strings.ReplaceAll(b.String(), "\u00a0", " ")
}

type gitwebTag struct{ Name, Ref, Action, Commit string }

func parseGitwebTags(b []byte, s GitSource) ([]gitwebTag, error) {
	if err := checkHTMLDocument(b); err != nil {
		return nil, err
	}
	doc, err := xhtml.Parse(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	tags := []gitwebTag{}
	var table *xhtml.Node
	htmlWalk(doc, func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "table" && htmlClass(n, "tags") && table == nil {
			table = n
		}
	})
	if table == nil {
		return nil, clientError("Gitweb repository did not return a tag table")
	}
	htmlWalk(table, func(n *xhtml.Node) {
		if n.Type != xhtml.ElementNode || n.Data != "tr" {
			return
		}
		var tag gitwebTag
		htmlWalk(n, func(a *xhtml.Node) {
			if a.Data != "a" {
				return
			}
			u, err := url.Parse(htmlAttr(a, "href"))
			if err != nil {
				return
			}
			q, err := gitwebQuery(u.RawQuery)
			if err != nil || q.Get("p") != s.Repo+".git" {
				return
			}
			if strings.HasPrefix(q.Get("h"), "refs/tags/") && (q.Get("a") == "shortlog" || q.Get("a") == "log" || q.Get("a") == "commit") {
				tag.Name = strings.TrimPrefix(q.Get("h"), "refs/tags/")
			}
			if q.Get("a") == "commit" && q.Get("h") != "" {
				tag.Commit = q.Get("h")
			}
			if q.Get("a") == "tag" {
				tag.Ref = q.Get("h")
				tag.Action = "tag"
			}
		})
		if tag.Name != "" {
			if tag.Ref == "" {
				tag.Ref = "refs/tags/" + tag.Name
				tag.Action = "commit"
			}
			tags = append(tags, tag)
		}
	})
	return tags, nil
}
func parseGitwebDetail(b []byte) (body, author string, date *time.Time, err error) {
	if err = checkHTMLDocument(b); err != nil {
		return "", "", nil, err
	}
	doc, e := xhtml.Parse(bytes.NewReader(b))
	if e != nil {
		return "", "", nil, e
	}
	htmlWalk(doc, func(n *xhtml.Node) {
		if n.Type != xhtml.ElementNode {
			return
		}
		if htmlClass(n, "page_body") {
			body = gitwebMessage(n)
		}
		if htmlClass(n, "datetime") && date == nil {
			v := strings.TrimSpace(htmlText(n))
			for _, layout := range []string{time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 -0700", time.RFC1123, time.RFC3339} {
				if t, e := time.Parse(layout, v); e == nil {
					date = &t
					break
				}
			}
		}
		if n.Data == "tr" {
			var cells []*xhtml.Node
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Data == "td" {
					cells = append(cells, c)
				}
			}
			if len(cells) >= 2 && strings.TrimSpace(htmlText(cells[0])) == "author" {
				author = strings.TrimSpace(htmlText(cells[1]))
			}
		}
	})
	// Signatures are metadata, not release notes.
	if i := strings.Index(body, "-----BEGIN PGP SIGNATURE-----"); i >= 0 {
		body = body[:i]
	}
	body = normalizeGitwebMessage(strings.TrimSpace(body))
	if date == nil {
		err = fmt.Errorf("gitweb tag/commit page has no valid source date")
	}
	return
}
func (p *Poller) fetchGitweb(ctx context.Context, s GitSource, id int64) (*gofeed.Feed, error) {
	budget := gitBudget{Limit: p.config.Poll.MaxBytes}
	b, err := p.gitGet(ctx, s.gitwebURL("tags", ""), "", &budget)
	if err != nil {
		return nil, err
	}
	tags, err := parseGitwebTags(b, s)
	if err != nil {
		return nil, err
	}
	feed := &gofeed.Feed{Title: s.title(), Link: s.URL}
	seen := map[string]bool{}
	selected := []gitwebTag{}
	choose := func(tag gitwebTag) {
		if seen[tag.Name] || !matchesTag(s.TagFilter, tag.Name) || (!s.Preview && isPreviewTag(tag.Name)) {
			return
		}
		seen[tag.Name] = true
		selected = append(selected, tag)
	}
	for _, tag := range tags {
		choose(tag)
		if len(selected) >= gitItemLimit {
			break
		}
	}
	for _, tag := range selected {
		preview := isPreviewTag(tag.Name)
		commitRef := tag.Commit
		if commitRef == "" {
			commitRef = "refs/tags/" + tag.Name
		}
		link := s.gitwebURL("commit", commitRef)
		var storedLink string
		var content, author string
		var published int64
		var storedObject string
		var storedVersion int
		object := tag.Ref + "\n" + commitRef
		// The commit URL also distinguishes older tag-only descriptions, which are
		// refreshed in place without changing entry IDs or read/starred state.
		err = p.store.DB.QueryRowContext(ctx, "SELECT content,author,published,url,git_object,render_version FROM entries WHERE feed_id=? AND guid=?", id, "git:"+tag.Name).Scan(&content, &author, &published, &storedLink, &storedObject, &storedVersion)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == sql.ErrNoRows || storedLink != link || storedObject != object || storedVersion != gitRenderVersion {
			b, err = p.gitGet(ctx, s.gitwebURL(tag.Action, tag.Ref), "", &budget)
			if err != nil {
				return nil, err
			}
			var body string
			var date *time.Time
			body, author, date, err = parseGitwebDetail(b)
			if err != nil {
				return nil, fmt.Errorf("gitweb tag %s: %w", tag.Name, err)
			}
			published = date.Unix()
			if tag.Action == "tag" {
				b, err = p.gitGet(ctx, link, "", &budget)
				if err != nil {
					return nil, fmt.Errorf("gitweb tag %s commit: %w", tag.Name, err)
				}
				commitBody, commitAuthor, _, detailErr := parseGitwebDetail(b)
				if detailErr != nil {
					return nil, fmt.Errorf("gitweb tag %s commit: %w", tag.Name, detailErr)
				}
				if author == "" {
					author = commitAuthor
				}
				if commitBody != "" && commitBody != body {
					if body == "" {
						body = commitBody
					} else {
						body += "\n\n## Commit\n\n" + commitBody
					}
				}
			}
			content, err = gitMarkdown(body, link)
			if err != nil {
				return nil, err
			}
		}
		date := time.Unix(published, 0)
		custom := map[string]string{"roi_git_object": object}
		if preview {
			custom["roi_preview"] = "1"
		}
		feed.Items = append(feed.Items, &gofeed.Item{GUID: "git:" + tag.Name, Title: tag.Name, Link: link, Content: content, PublishedParsed: &date, UpdatedParsed: &date, Author: &gofeed.Person{Name: author}, Custom: custom})
		if len(feed.Items) >= gitItemLimit {
			break
		}
	}
	sort.SliceStable(feed.Items, func(i, j int) bool { return feed.Items[i].PublishedParsed.After(*feed.Items[j].PublishedParsed) })
	return feed, nil
}

// Release commits often indent their entire change list by four spaces.
// Remove that wrapper indentation while preserving nesting and continuations.
func normalizeGitwebMessage(body string) string {
	lines := strings.Split(body, "\n")
	list := false
	for i, line := range lines {
		if strings.HasPrefix(line, "    - ") || strings.HasPrefix(line, "    * ") || strings.HasPrefix(line, "    + ") {
			list = true
		}
		if list && strings.HasPrefix(line, "    ") {
			lines[i] = line[4:]
		} else if strings.TrimSpace(line) != "" {
			list = false
		}
	}
	return strings.Join(lines, "\n")
}
