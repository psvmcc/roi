package roi

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const gitFolder = "GIT_Releases"
const gitReposFolder = "GIT_Repos"
const kindRSS = "rss"
const kindGitReleases = "git_releases"
const kindGitRepos = "git_repos"

const readerEntryFilter = "f.active=1 AND (f.source_kind<>'git_repos' OR f.git_ignore_forks=0 OR e.git_fork=0) AND (f.git_preview=1 OR e.git_preview=0) AND (f.git_tag_filter='' OR CASE WHEN f.git_tag_filter_mode='regex' THEN e.git_tag_match ELSE substr(e.guid,5) GLOB f.git_tag_filter END)"

const gitPageSize = 100

const gitMaxPages = 5

const gitItemLimit = 20

type GitSource struct {
	Kind, OwnerType                 string
	OwnerID                         int64
	IncludeSubgroups                bool
	IgnoreForks                     bool
	URL, Provider, Repo, Mode, Icon string
	TagFilter                       string
	TagFilterMode                   string
	Preview                         bool
	Revision                        int64
}

func reservedGitFolder(name string) bool {
	return name == gitFolder || strings.HasPrefix(name, gitFolder+"/") || name == gitReposFolder || strings.HasPrefix(name, gitReposFolder+"/")
}

func parseGitSource(raw, provider, mode string, preview bool) (GitSource, error) {
	s := GitSource{Provider: provider, Mode: mode, Preview: preview}
	if len(raw) > 4096 {
		return s, clientError("repository URL is too long")
	}
	if err := validFeedURL(raw); err != nil {
		return s, clientError(err.Error())
	}
	u, _ := url.Parse(raw)
	if provider == "gitweb" || (provider == "auto" && u.RawQuery != "") {
		return parseGitwebSource(u, mode, preview)
	}
	if u.RawQuery != "" || u.RawPath != "" {
		return s, clientError("use a repository URL without query parameters or encoded path components")
	}
	u.Host = strings.ToLower(u.Host)
	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return s, clientError("repository URL must include an owner and repository")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\?#%\x00\r\n\t ") {
			return s, clientError("invalid repository path")
		}
	}
	switch provider {
	case "github", "gitea", "forgejo":
		if len(parts) != 2 {
			return s, clientError("this service requires an owner/repository URL")
		}
	case "gitlab", "auto":
	default:
		return s, clientError("unsupported Git service")
	}
	if mode != "releases" && mode != "tags" {
		return s, clientError("choose releases or tags")
	}
	u.Path = "/" + path
	u.RawPath = ""
	s.URL = u.String()
	s.Repo = path
	s.Icon = u.Scheme + "://" + u.Host + "/favicon.ico"
	return s, nil
}

func (s GitSource) title() string { u, _ := url.Parse(s.URL); return u.Host + " :: " + s.Repo }

func (s GitSource) origin() string { u, _ := url.Parse(s.URL); return u.Scheme + "://" + u.Host }

func (s GitSource) apiBase() string {
	if s.Provider == "github" {
		u, _ := url.Parse(s.URL)
		if u.Host == "github.com" {
			return "https://api.github.com"
		}
		return s.origin() + "/api/v3"
	}
	if s.Provider == "gitlab" {
		return s.origin() + "/api/v4"
	}
	return s.origin() + "/api/v1"
}

func (s GitSource) endpoint() string {
	if s.Provider == "gitlab" {
		kind := "releases"
		if s.Mode == "tags" {
			kind = "repository/tags"
		}
		return s.apiBase() + "/projects/" + url.PathEscape(s.Repo) + "/" + kind
	}
	parts := strings.Split(s.Repo, "/")
	return s.apiBase() + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/" + s.Mode
}

func tagFilterMode(modes []string) string {
	if len(modes) == 0 || modes[0] == "" {
		return "glob"
	}
	return modes[0]
}

// Both syntaxes match the complete tag. Go regexp has linear-time matching.
func compileTagFilter(pattern, mode string) (*regexp.Regexp, error) {
	if mode == "" {
		mode = "glob"
	}
	if mode != "glob" && mode != "regex" {
		return nil, clientError("tag filter mode must be glob or regex")
	}
	if len(pattern) > 256 || !utf8.ValidString(pattern) || strings.ContainsFunc(pattern, unicode.IsControl) {
		return nil, clientError("tag filter must be valid UTF-8, at most 256 bytes and contain no control characters")
	}
	if mode == "glob" && strings.ContainsAny(pattern, "[]\\") {
		return nil, clientError("glob tag filters support * and ? wildcards, without brackets or backslashes")
	}
	if pattern == "" {
		return nil, nil
	}
	expression := pattern
	if mode == "glob" {
		expression = regexp.QuoteMeta(pattern)
		expression = strings.ReplaceAll(expression, `\*`, `.*`)
		expression = strings.ReplaceAll(expression, `\?`, `.`)
		expression = "(?s:" + expression + ")"
	}
	re, err := regexp.Compile("\\A(?:" + expression + ")\\z")
	if err != nil {
		return nil, clientError("invalid tag filter regex: " + err.Error())
	}
	return re, nil
}
func validateTagFilter(pattern string, modes ...string) error {
	_, err := compileTagFilter(pattern, tagFilterMode(modes))
	return err
}
func matchesTag(pattern, tag string, modes ...string) bool {
	re, err := compileTagFilter(pattern, tagFilterMode(modes))
	return err == nil && (re == nil || re.MatchString(tag))
}
