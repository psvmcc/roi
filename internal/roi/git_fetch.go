package roi

import (
	"context"
	"net/url"
	"sort"
	"strconv"

	"github.com/mmcdole/gofeed"
)

// Only the latest matching versions are polled. Pagination is bounded and used
// solely to fill this window after filtering preview or nonmatching versions.
func (p *Poller) fetchGit(ctx context.Context, s GitSource, id int64) (*gofeed.Feed, error) {
	ctx, cancel := context.WithTimeout(ctx, p.config.timeout())
	defer cancel()
	ctx = context.WithValue(ctx, gitCacheScope{}, gitScope(id, s.Revision))
	if s.Provider == "gitweb" {
		return p.fetchGitweb(ctx, s, id)
	}
	budget := gitBudget{Limit: p.config.Poll.MaxBytes}
	feed := &gofeed.Feed{Title: s.title(), Link: s.URL}
	seen := map[string]bool{}
	for page := 1; page <= gitMaxPages && len(feed.Items) < gitItemLimit; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}}
		if s.Provider == "gitea" || s.Provider == "forgejo" {
			q.Set("limit", strconv.Itoa(gitItemLimit))
		} else {
			q.Set("per_page", strconv.Itoa(gitItemLimit))
		}
		hasNext := false
		b, err := p.gitGet(ctx, s.endpoint()+"?"+q.Encode(), p.gitToken(s), &budget, &hasNext)
		if err != nil {
			return nil, err
		}
		items, err := decodeGitList(b)
		if err != nil {
			return nil, err
		}
		for _, release := range items {
			tag := release.Tag
			if s.Mode == "tags" {
				tag = release.Name
			}
			if seen[tag] {
				continue
			}
			seen[tag] = true
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
		if len(items) < gitItemLimit && !hasNext {
			break
		}
	}
	sort.SliceStable(feed.Items, func(i, j int) bool { return feed.Items[i].PublishedParsed.After(*feed.Items[j].PublishedParsed) })
	return feed, nil
}
