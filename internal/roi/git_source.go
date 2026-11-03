package roi

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const gitFolder = "GIT_Releases"

const readerEntryFilter = "f.active=1 AND (f.git_preview=1 OR e.git_preview=0) AND (f.git_tag_filter='' OR substr(e.guid,5) GLOB f.git_tag_filter)"

const gitPageSize = 100

const gitMaxPages = 5

const gitItemLimit = 20

type GitSource struct {
	URL, Provider, Repo, Mode, Icon string
	TagFilter                       string
	Preview                         bool
	Revision                        int64
}

func reservedGitFolder(name string) bool {
	return name == gitFolder || strings.HasPrefix(name, gitFolder+"/")
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

// Tag filters are case-sensitive globs, with no path-separator semantics.
func validateTagFilter(pattern string) error {
	if len(pattern) > 256 || !utf8.ValidString(pattern) || strings.ContainsFunc(pattern, unicode.IsControl) || strings.ContainsAny(pattern, "[]\\") {
		return clientError("tag filter must be at most 256 bytes; use * and ? wildcards, without brackets, backslashes or control characters")
	}
	return nil
}
func matchesTag(pattern, tag string) bool {
	if pattern == "" {
		return true
	}
	expression := regexp.QuoteMeta(pattern)
	expression = strings.ReplaceAll(expression, `\*`, `.*`)
	expression = strings.ReplaceAll(expression, `\?`, `.`)
	matched, _ := regexp.MatchString("(?s)^(?:"+expression+")$", tag)
	return matched
}
