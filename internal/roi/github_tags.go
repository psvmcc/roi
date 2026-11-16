package roi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mmcdole/gofeed"
)

// Ref ordering is by the underlying commit's date, not the tag's name or
// publication date. A bounded unroll supports tags pointing to other tags.
const githubCommitFields = "oid committedDate message author { name }"

var githubTagsQuery = func() string {
	fields := "__typename ... on Commit { " + githubCommitFields + " }"
	for range 4 {
		fields = "__typename ... on Commit { " + githubCommitFields + " } ... on Tag { oid message tagger { name } target { " + fields + " } }"
	}
	return `query RoiTags($owner: String!, $name: String!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
   refs(refPrefix: "refs/tags/", first: $first, after: $after, orderBy: {field: TAG_COMMIT_DATE, direction: DESC}) {
    nodes { name target { ` + fields + ` } }
    pageInfo { hasNextPage endCursor }
   }
  }
  rateLimit { remaining resetAt }
 }`
}()

type githubGitObject struct {
	Type    string `json:"__typename"`
	OID     string `json:"oid"`
	Date    string `json:"committedDate"`
	Message string `json:"message"`
	Author  struct {
		Name string `json:"name"`
	} `json:"author"`
	Tagger struct {
		Name string `json:"name"`
	} `json:"tagger"`
	Target *githubGitObject `json:"target"`
}
type githubTagRef struct {
	Name   string           `json:"name"`
	Target *githubGitObject `json:"target"`
}
type githubTagPage struct {
	Nodes    []githubTagRef `json:"nodes"`
	PageInfo *struct {
		Next   bool   `json:"hasNextPage"`
		Cursor string `json:"endCursor"`
	} `json:"pageInfo"`
}
type githubTagsResponse struct {
	Data *struct {
		Repository *struct {
			Refs *githubTagPage `json:"refs"`
		} `json:"repository"`
		RateLimit *struct {
			Remaining *int   `json:"remaining"`
			Reset     string `json:"resetAt"`
		} `json:"rateLimit"`
	} `json:"data"`
	Errors []struct {
		Type       string `json:"type"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

func (s GitSource) githubGraphQLURL() string {
	u, _ := url.Parse(s.URL)
	if strings.EqualFold(u.Host, "github.com") {
		return "https://api.github.com/graphql"
	}
	return s.origin() + "/api/graphql"
}
func (p *Poller) githubTagsPage(ctx context.Context, s GitSource, cursor string, first int, budget *gitBudget) (*githubTagPage, error) {
	token := p.gitToken(s)
	if token == "" {
		return nil, clientError("GitHub tags ordered by date require a token in git.tokens")
	}
	if first < 1 || first > gitPageSize {
		return nil, fmt.Errorf("invalid GitHub tag page size")
	}
	parts := strings.Split(s.Repo, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid GitHub repository")
	}
	var after *string
	if cursor != "" {
		after = &cursor
	}
	payload, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{githubTagsQuery, map[string]any{"owner": parts[0], "name": parts[1], "first": first, "after": after}})
	if err != nil {
		return nil, err
	}
	endpoint := s.githubGraphQLURL()
	body, err := p.gitPost(ctx, endpoint, token, payload, budget)
	if err != nil {
		return nil, err
	}
	if err = checkJSONDocument(body); err != nil {
		return nil, err
	}
	var response githubTagsResponse
	if err = json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	limited := false
	for _, issue := range response.Errors {
		if issue.Type == "RATE_LIMITED" || issue.Extensions.Code == "RATE_LIMITED" {
			limited = true
		}
	}
	if limited || response.Data != nil && response.Data.RateLimit != nil && response.Data.RateLimit.Remaining != nil && *response.Data.RateLimit.Remaining == 0 {
		headers := http.Header{"X-RateLimit-Remaining": {"0"}}
		if response.Data != nil && response.Data.RateLimit != nil {
			if reset := gitDate(response.Data.RateLimit.Reset); reset != nil {
				headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			}
		}
		u, _ := url.Parse(endpoint)
		retry := p.observeHostResponse(ctx, hostGroup(u), &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
		if limited {
			return nil, &gitHTTPError{Status: http.StatusTooManyRequests, Retry: max(retry, p.hostCooldown(hostGroup(u)))}
		}
	}
	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("GitHub GraphQL query failed; check token permissions and repository access")
	}
	if response.Data == nil || response.Data.Repository == nil || response.Data.Repository.Refs == nil {
		return nil, fmt.Errorf("GitHub GraphQL repository or tag list is unavailable")
	}
	page := response.Data.Repository.Refs
	if page.Nodes == nil || page.PageInfo == nil || len(page.Nodes) > first {
		return nil, fmt.Errorf("GitHub GraphQL returned an invalid tag page")
	}
	if page.PageInfo.Next && (page.PageInfo.Cursor == "" || page.PageInfo.Cursor == cursor || len(page.PageInfo.Cursor) > 4096) {
		return nil, fmt.Errorf("GitHub GraphQL returned an invalid pagination cursor")
	}
	return page, nil
}

func githubTagRelease(ref githubTagRef) (gitRelease, error) {
	release := gitRelease{Name: ref.Name}
	object := ref.Target
	for depth := 0; object != nil && object.Type == "Tag" && depth < 4; depth++ {
		if release.Message == "" {
			release.Message = object.Message
		}
		if release.Author.Name == "" {
			release.Author.Name = object.Tagger.Name
		}
		object = object.Target
	}
	if object == nil || object.Type != "Commit" {
		return release, fmt.Errorf("GitHub tag has no supported commit target")
	}
	if object.OID == "" || gitDate(object.Date) == nil {
		return release, fmt.Errorf("GitHub tag is missing its commit ID or date")
	}
	release.Commit.SHA = object.OID
	release.Commit.Created = object.Date
	release.Commit.Message = object.Message
	if release.Author.Name == "" {
		release.Author.Name = object.Author.Name
	}
	return release, nil
}

func (p *Poller) fetchGitHubTags(ctx context.Context, s GitSource, id int64) (*gofeed.Feed, error) {
	budget := gitBudget{Limit: p.config.Poll.MaxBytes}
	feed := &gofeed.Feed{Title: s.title(), Link: s.URL}
	seen := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for pageNo := 0; pageNo < gitMaxPages && len(feed.Items) < gitItemLimit; pageNo++ {
		page, err := p.githubTagsPage(ctx, s, cursor, gitPageSize, &budget)
		if err != nil {
			return nil, err
		}
		for _, ref := range page.Nodes {
			if ref.Name == "" || seen[ref.Name] || !matchesTag(s.TagFilter, ref.Name, s.TagFilterMode) || !s.Preview && isPreviewTag(ref.Name) {
				continue
			}
			seen[ref.Name] = true
			release, err := githubTagRelease(ref)
			if err != nil {
				return nil, err
			}
			item, err := p.gitItem(ctx, s, id, release, &budget)
			if err != nil {
				return nil, err
			}
			if item != nil {
				feed.Items = append(feed.Items, item)
			}
			if len(feed.Items) >= gitItemLimit {
				break
			}
		}
		if !page.PageInfo.Next || len(feed.Items) >= gitItemLimit {
			break
		}
		cursor = page.PageInfo.Cursor
		if cursors[cursor] {
			return nil, fmt.Errorf("GitHub GraphQL pagination cursor repeated")
		}
		cursors[cursor] = true
	}
	sort.SliceStable(feed.Items, func(i, j int) bool { return feed.Items[i].PublishedParsed.After(*feed.Items[j].PublishedParsed) })
	return feed, nil
}
