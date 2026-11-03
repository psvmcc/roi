package roi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"time"

	"github.com/mmcdole/gofeed"
)

var previewTagPattern = regexp.MustCompile(`(?i)(^|[._/\-]|[0-9])(alpha|beta|rc|pre|preview|test|testing|dev|snapshot|nightly|canary|experimental)([0-9._/\-]|$)`)

func isPreviewTag(tag string) bool { return previewTagPattern.MatchString(tag) }

type gitRelease struct {
	ID          json.RawMessage `json:"id"`
	Name        string          `json:"name"`
	Tag         string          `json:"tag_name"`
	Body        string          `json:"body"`
	Description string          `json:"description"`
	Message     string          `json:"message"`
	HTMLURL     string          `json:"html_url"`
	Draft       bool            `json:"draft"`
	Prerelease  bool            `json:"prerelease"`
	Upcoming    bool            `json:"upcoming_release"`
	Published   string          `json:"published_at"`
	Released    string          `json:"released_at"`
	Created     string          `json:"created_at"`
	Author      struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	} `json:"author"`
	Commit struct {
		SHA       string `json:"sha"`
		ID        string `json:"id"`
		Created   string `json:"created"`
		CreatedAt string `json:"created_at"`
		Committed string `json:"committed_date"`
		Timestamp string `json:"timestamp"`
		Message   string `json:"message"`
	} `json:"commit"`
	Links struct {
		Self string `json:"self"`
	} `json:"_links"`
	Release *struct {
		Description string `json:"description"`
	} `json:"release"`
}

func gitDate(values ...string) *time.Time {
	for _, v := range values {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil && !t.IsZero() {
			return &t
		}
	}
	return nil
}

func (p *Poller) gitItem(ctx context.Context, s GitSource, id int64, release gitRelease, budget *gitBudget) (*gofeed.Item, error) {
	tag := release.Tag
	if s.Mode == "tags" {
		tag = release.Name
	}
	preview := release.Prerelease || release.Upcoming || isPreviewTag(tag)
	if release.Draft || tag == "" || !matchesTag(s.TagFilter, tag) || (!s.Preview && preview) {
		return nil, nil
	}
	date := gitDate(release.Published, release.Released, release.Created, release.Commit.CreatedAt, release.Commit.Created, release.Commit.Committed, release.Commit.Timestamp)
	cachedContent := ""
	body := release.Body
	if body == "" {
		body = release.Description
	}
	name := release.Name
	link := release.HTMLURL
	if s.Provider == "gitlab" {
		link = release.Links.Self
	}
	if s.Mode == "tags" {
		name = tag
		body = release.Message
		if release.Release != nil && release.Release.Description != "" {
			body = release.Release.Description
		}
		if body == "" {
			body = release.Commit.Message
		}
		link = s.URL + "/src/tag/" + url.PathEscape(tag)
		if s.Provider == "github" {
			link = s.URL + "/tree/" + url.PathEscape(tag)
		}
		if s.Provider == "gitlab" {
			link = s.URL + "/-/tags/" + url.PathEscape(tag)
		}
		if s.Provider == "github" && date == nil {
			// Reuse a stored tag date: GitHub's tag list omits timestamps.
			var stored int64
			var object string
			var version int
			sha := release.Commit.SHA
			if sha == "" {
				return nil, fmt.Errorf("GitHub tag has no commit SHA")
			}
			err := p.store.DB.QueryRowContext(ctx, "SELECT published,content,git_object,render_version FROM entries WHERE feed_id=? AND guid=?", id, "git:"+tag).Scan(&stored, &cachedContent, &object, &version)
			if err == nil && object == sha && version == gitRenderVersion {
				t := time.Unix(stored, 0)
				date = &t
			} else if err != nil && err != sql.ErrNoRows {
				return nil, err
			} else {
				cachedContent = ""
				raw := s.apiBase() + "/repos/" + s.Repo + "/commits/" + url.PathEscape(sha)
				b, err := p.gitGet(ctx, raw, p.gitToken(s), budget)
				if err != nil {
					return nil, err
				}
				var commit struct {
					Commit struct {
						Message   string
						Author    struct{ Name, Date string }
						Committer struct{ Date string }
					}
				}
				if err = json.Unmarshal(b, &commit); err != nil {
					return nil, err
				}
				date = gitDate(commit.Commit.Committer.Date, commit.Commit.Author.Date)
				if body == "" {
					body = commit.Commit.Message
				}
			}
		}
	}
	if date == nil {
		return nil, nil
	} // Never substitute a poll timestamp for an unknown source date.
	if name == "" {
		name = tag
	}
	if link == "" {
		link = s.URL + "/releases/tag/" + url.PathEscape(tag)
		if s.Provider == "gitlab" {
			link = s.URL + "/-/releases/" + url.PathEscape(tag)
		}
	}
	if err := validFeedURL(link); err != nil {
		return nil, fmt.Errorf("git API returned an invalid release URL")
	}
	content, err := gitMarkdown(body, link)
	if body == "" && cachedContent != "" {
		content = cachedContent
	}
	if err != nil {
		return nil, err
	}
	author := release.Author.Login
	if author == "" {
		author = release.Author.Name
	}
	if author == "" {
		author = s.Provider
	}
	object := release.Commit.SHA
	if object == "" {
		object = release.Commit.ID
	}
	if object == "" {
		object = "release:" + tag
	}
	custom := map[string]string{"roi_git_object": object}
	if preview {
		custom["roi_preview"] = "1"
	}
	return &gofeed.Item{GUID: "git:" + tag, Title: name, Link: link, Content: content, PublishedParsed: date, UpdatedParsed: date, Author: &gofeed.Person{Name: author}, Custom: custom}, nil
}

func decodeGitList(b []byte) ([]gitRelease, error) {
	if err := checkJSONDocument(b); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("git API did not return an array")
	}
	items := make([]gitRelease, 0, gitPageSize)
	for decoder.More() {
		if len(items) >= gitPageSize {
			return nil, fmt.Errorf("git API page exceeds %d entries", gitPageSize)
		}
		var item gitRelease
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("invalid trailing Git API data")
	}
	return items, nil
}
