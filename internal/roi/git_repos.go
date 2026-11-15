package roi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mmcdole/gofeed"
)

const gitReposLimit = 100

type gitAccount struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	Username string `json:"username"`
	FullPath string `json:"full_path"`
	Type     string `json:"type"`
	Avatar   string `json:"avatar_url"`
	HTMLURL  string `json:"html_url"`
	WebURL   string `json:"web_url"`
}
type gitRepoRecord struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	HTMLURL     string     `json:"html_url"`
	WebURL      string     `json:"web_url"`
	Created     string     `json:"created_at"`
	Private     bool       `json:"private"`
	Visibility  string     `json:"visibility"`
	Owner       gitAccount `json:"owner"`
}

func parseGitReposSource(raw, provider, ownerType string, subgroups bool) (GitSource, error) {
	if provider == "" {
		provider = "auto"
	}
	s := GitSource{Kind: kindGitRepos, Provider: provider, OwnerType: ownerType, IncludeSubgroups: subgroups}
	if len(raw) > 4096 {
		return s, clientError("account URL is too long")
	}
	if err := validFeedURL(raw); err != nil {
		return s, clientError(err.Error())
	}
	u, _ := url.Parse(raw)
	if u.RawQuery != "" || u.RawPath != "" || u.Fragment != "" {
		return s, clientError("use an account URL without query parameters or encoded path components")
	}
	u.Host = strings.ToLower(u.Host)
	path := strings.Trim(u.Path, "/")
	if provider == "gitlab" {
		path = strings.TrimPrefix(path, "groups/")
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\?#%\x00\r\n\t ") {
			return s, clientError("invalid Git account path")
		}
	}
	switch provider {
	case "auto", "github", "gitlab", "forgejo", "gitea":
	default:
		return s, clientError("choose GitHub, GitLab, Forgejo or Gitea")
	}
	if provider != "auto" && provider != "gitlab" && len(parts) != 1 {
		return s, clientError("use a user or organization URL, not a repository URL")
	}
	if ownerType == "" {
		s.OwnerType = "auto"
	}
	switch s.OwnerType {
	case "auto", "user", "organization", "group":
	default:
		return s, clientError("invalid account type")
	}
	if provider != "auto" && provider != "gitlab" && (s.OwnerType == "group" || subgroups) {
		return s, clientError("groups and subgroups are supported only for GitLab")
	}
	if provider == "gitlab" && s.OwnerType == "organization" {
		return s, clientError("choose group for a GitLab organization")
	}
	if subgroups && s.OwnerType == "user" {
		return s, clientError("subgroups cannot be enabled for a user")
	}
	u.Path = "/" + path
	u.RawPath = ""
	s.URL = u.String()
	s.Repo = path
	s.Icon = s.origin() + "/favicon.ico"
	return s, nil
}
func gitFolderFor(kind string) string {
	if kind == kindGitRepos {
		return gitReposFolder
	}
	return gitFolder
}
func decodeRepoJSON(data []byte, target any) error {
	if err := checkJSONDocument(data); err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
func gitNotFound(err error) bool {
	var status *gitHTTPError
	return errors.As(err, &status) && status.Status == http.StatusNotFound
}

func (p *Poller) detectReposProvider(ctx context.Context, s GitSource, budget *gitBudget) (GitSource, error) {
	if s.Provider != "auto" {
		return s, nil
	}
	u, _ := url.Parse(s.URL)
	switch strings.ToLower(u.Hostname()) {
	case "github.com":
		s.Provider = "github"
	case "gitlab.com":
		s.Provider = "gitlab"
	case "codeberg.org":
		s.Provider = "forgejo"
	}
	if s.Provider != "auto" {
		return s, nil
	}
	for _, probe := range []struct{ provider, path string }{{"forgejo", "/api/forgejo/v1/version"}, {"gitea", "/api/v1/version"}, {"gitlab", "/api/v4/version"}} {
		data, err := p.gitGet(ctx, s.origin()+probe.path, p.gitToken(s), budget)
		if err != nil {
			if gitNotFound(err) {
				continue
			}
			var status *gitHTTPError
			if errors.As(err, &status) && (status.Status == 401 || status.Status == 403) {
				continue
			}
			return s, err
		}
		var version struct {
			Version string `json:"version"`
		}
		if decodeRepoJSON(data, &version) == nil && version.Version != "" {
			s.Provider = probe.provider
			return s, nil
		}
	}
	return s, clientError("cannot detect Git service; select it explicitly")
}
func (p *Poller) loadGitAccount(ctx context.Context, s GitSource, budget *gitBudget) (gitAccount, string, error) {
	var account gitAccount
	get := func(endpoint string, out any) error {
		data, err := p.gitGet(ctx, s.apiBase()+endpoint, p.gitToken(s), budget)
		if err != nil {
			return err
		}
		return decodeRepoJSON(data, out)
	}
	kind := s.OwnerType
	if kind == "" {
		kind = "auto"
	}
	if s.Provider == "github" {
		if err := get("/users/"+url.PathEscape(s.Repo), &account); err != nil {
			return account, "", err
		}
		actual := "user"
		if strings.EqualFold(account.Type, "Organization") {
			actual = "organization"
		}
		if kind != "auto" && kind != actual {
			return account, "", clientError("account type does not match the GitHub profile")
		}
		kind = actual
	} else if s.Provider == "gitlab" {
		if kind == "auto" || kind == "group" {
			target := url.PathEscape(s.Repo)
			if kind == "group" && s.OwnerID > 0 {
				target = strconv.FormatInt(s.OwnerID, 10)
			}
			err := get("/groups/"+target+"?with_projects=false", &account)
			if err == nil {
				kind = "group"
			} else if kind != "auto" || !gitNotFound(err) {
				return account, "", err
			}
		}
		if kind != "group" {
			if s.OwnerID > 0 && kind == "user" {
				if err := get("/users/"+strconv.FormatInt(s.OwnerID, 10), &account); err != nil {
					return account, "", err
				}
			} else {
				var users []gitAccount
				if err := get("/users?"+url.Values{"username": {s.Repo}}.Encode(), &users); err != nil {
					return account, "", err
				}
				found := false
				for _, user := range users {
					if strings.EqualFold(user.Username, s.Repo) {
						account = user
						found = true
						break
					}
				}
				if !found {
					return account, "", clientError("GitLab user not found")
				}
			}
			kind = "user"
		}
	} else {
		if kind == "auto" || kind == "organization" {
			err := get("/orgs/"+url.PathEscape(s.Repo), &account)
			if err == nil {
				kind = "organization"
			} else if kind != "auto" || !gitNotFound(err) {
				return account, "", err
			}
		}
		if kind != "organization" {
			if err := get("/users/"+url.PathEscape(s.Repo), &account); err != nil {
				return account, "", err
			}
			kind = "user"
		}
	}
	if account.ID <= 0 {
		return account, "", clientError("Git account response is missing its ID")
	}
	if s.OwnerID > 0 && account.ID != s.OwnerID {
		return account, "", clientError("account identity changed; verify this subscription in the admin")
	}
	if s.IncludeSubgroups && kind != "group" {
		return account, "", clientError("include subgroups requires a GitLab group")
	}
	return account, kind, nil
}
func accountName(account gitAccount, fallback string) string {
	for _, value := range []string{account.FullPath, account.Login, account.Username} {
		if value != "" {
			return value
		}
	}
	return fallback
}
func accountWebsite(account gitAccount, fallback string) string {
	for _, value := range []string{account.HTMLURL, account.WebURL} {
		if validFeedURL(value) == nil {
			return value
		}
	}
	return fallback
}
func accountAvatar(account gitAccount, website, fallback string) string {
	if icon := resolvedIcon(website, account.Avatar); icon != "" {
		return icon
	}
	return fallback
}

func (p *Poller) discoverGitRepos(ctx context.Context, s GitSource) (GitSource, error) {
	ctx, cancel := context.WithTimeout(ctx, p.config.timeout())
	defer cancel()
	if err := p.resources.acquire(ctx, 2*p.config.Poll.MaxBytes); err != nil {
		return s, err
	}
	defer p.resources.release(2 * p.config.Poll.MaxBytes)
	u, _ := url.Parse(s.URL)
	host := hostGroup(u)
	if err := p.waitHost(ctx, host); err != nil {
		return s, err
	}
	lease := &hostLease{keys: map[string]bool{host: true}}
	ctx = context.WithValue(ctx, hostLeaseKey{}, lease)
	defer func() {
		for key := range lease.keys {
			p.releaseHost(key)
		}
	}()
	budget := gitBudget{Limit: p.config.Poll.MaxBytes}
	var err error
	s, err = p.detectReposProvider(ctx, s, &budget)
	if err != nil {
		return s, err
	}
	checked, err := parseGitReposSource(s.URL, s.Provider, s.OwnerType, s.IncludeSubgroups)
	if err != nil {
		return s, err
	}
	profile, kind, err := p.loadGitAccount(ctx, checked, &budget)
	if err != nil {
		return s, err
	}
	name := accountName(profile, checked.Repo)
	canonical := checked.origin() + "/" + name
	checked, err = parseGitReposSource(canonical, checked.Provider, kind, checked.IncludeSubgroups)
	if err != nil {
		return s, err
	}
	checked.OwnerID = profile.ID
	checked.Icon = accountAvatar(profile, accountWebsite(profile, canonical), checked.Icon)
	return checked, nil
}
func (s GitSource) reposEndpoint(account gitAccount, kind string, page int) string {
	q := url.Values{"page": {strconv.Itoa(page)}}
	var endpoint string
	switch s.Provider {
	case "github":
		endpoint = "/users/" + url.PathEscape(accountName(account, s.Repo)) + "/repos"
		q.Set("sort", "created")
		q.Set("direction", "desc")
		q.Set("per_page", "100")
	case "gitlab":
		endpoint = "/users/" + strconv.FormatInt(account.ID, 10) + "/projects"
		if kind == "group" {
			endpoint = "/groups/" + strconv.FormatInt(account.ID, 10) + "/projects"
			q.Set("with_shared", "false")
			q.Set("include_subgroups", strconv.FormatBool(s.IncludeSubgroups))
		}
		q.Set("order_by", "created_at")
		q.Set("sort", "desc")
		q.Set("per_page", "100")
		q.Set("visibility", "public")
	default:
		endpoint = "/repos/search"
		q.Set("uid", strconv.FormatInt(account.ID, 10))
		q.Set("exclusive", "true")
		q.Set("sort", "created")
		q.Set("order", "desc")
		q.Set("private", "false")
		q.Set("limit", "100")
	}
	return s.apiBase() + endpoint + "?" + q.Encode()
}
func (p *Poller) fetchGitRepos(ctx context.Context, s GitSource, id int64) (*gofeed.Feed, error) {
	ctx, cancel := context.WithTimeout(ctx, p.config.timeout())
	defer cancel()
	ctx = context.WithValue(ctx, gitCacheScope{}, gitScope(id, s.Revision))
	budget := gitBudget{Limit: p.config.Poll.MaxBytes}
	profile, kind, err := p.loadGitAccount(ctx, s, &budget)
	if err != nil {
		return nil, err
	}
	website := accountWebsite(profile, s.URL)
	icon := accountAvatar(profile, website, s.Icon)
	feed := &gofeed.Feed{Title: s.title(), Link: website, Custom: map[string]string{"roi_owner_id": strconv.FormatInt(profile.ID, 10), "roi_owner_type": kind, "roi_owner_avatar": icon}}
	seen := map[int64]bool{}
	for page := 1; page <= gitMaxPages; page++ {
		hasNext := false
		data, err := p.gitGet(ctx, s.reposEndpoint(profile, kind, page), p.gitToken(s), &budget, &hasNext)
		if err != nil {
			return nil, err
		}
		var repos []gitRepoRecord
		if s.Provider == "forgejo" || s.Provider == "gitea" {
			var result struct {
				Data json.RawMessage `json:"data"`
			}
			if err = decodeRepoJSON(data, &result); err != nil {
				return nil, err
			}
			data = result.Data
		}
		if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '[' {
			return nil, clientError("repository response must contain an array of repositories")
		}
		if err = decodeRepoJSON(data, &repos); err != nil {
			return nil, err
		}

		for _, repo := range repos {
			if repo.ID <= 0 || seen[repo.ID] || repo.Private || repo.Visibility != "" && repo.Visibility != "public" {
				continue
			}
			if s.Provider != "gitlab" && repo.Owner.ID > 0 && repo.Owner.ID != profile.ID {
				continue
			}
			date := gitDate(repo.Created)
			link := repo.HTMLURL
			if link == "" {
				link = repo.WebURL
			}
			if date == nil || repo.Name == "" || validFeedURL(link) != nil {
				continue
			}
			seen[repo.ID] = true
			description := repo.Description
			if len(description) > 64<<10 {
				description = strings.ToValidUTF8(description[:64<<10], "�")
			}
			body := ""
			if description != "" {
				body = "<p>" + strings.ReplaceAll(html.EscapeString(description), "\n", "<br>") + "</p>"
			}
			feed.Items = append(feed.Items, &gofeed.Item{GUID: "gitrepo:" + strconv.FormatInt(repo.ID, 10), Title: repo.Name, Link: link, Description: body, Content: body, PublishedParsed: date, Author: &gofeed.Person{Name: accountName(profile, s.Repo)}})
			if len(feed.Items) >= gitReposLimit {
				break
			}
		}
		if len(feed.Items) >= gitReposLimit || !hasNext {
			break
		}
		if page == gitMaxPages {
			feed.Custom["roi_repos_partial"] = "1"
		}
	}
	sort.SliceStable(feed.Items, func(i, j int) bool { return feed.Items[i].PublishedParsed.After(*feed.Items[j].PublishedParsed) })
	return feed, nil
}
