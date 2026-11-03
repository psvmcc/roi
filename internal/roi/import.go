package roi

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

type importSubscription struct {
	Entry  roiSource
	Source GitSource
}
type ImportItem struct {
	Index  int    `json:"index"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
type ImportPreview struct {
	Total   int          `json:"total"`
	Add     int          `json:"add"`
	Skipped int          `json:"skipped"`
	Invalid int          `json:"invalid"`
	Items   []ImportItem `json:"items"`
}

func decodeSubscriptions(r io.Reader, format string) ([]roiSource, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxROIBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxROIBytes {
		return nil, fmt.Errorf("import exceeds 8 MiB")
	}
	if format == "roi" {
		if err := checkYAMLDocument(data); err != nil {
			return nil, err
		}
		d := yaml.NewDecoder(bytes.NewReader(data))
		d.KnownFields(true)
		var file roiFile
		if err = d.Decode(&file); err != nil {
			return nil, err
		}
		if file.Format != "roi" || file.Version != 1 {
			return nil, fmt.Errorf("unsupported ROI format/version (expected roi version 1)")
		}
		if err = d.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("ROI must contain one YAML document")
		}
		if len(file.Sources) > 10000 {
			return nil, fmt.Errorf("import contains more than 10000 sources")
		}
		return file.Sources, nil
	}
	if format != "opml" {
		return nil, fmt.Errorf("unknown import format")
	}
	if err := checkFeedDocument(data); err != nil {
		return nil, err
	}
	var file OPML
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if err = decoder.Decode(&file); err != nil {
		return nil, err
	}
	// Reject trailing XML elements instead of silently importing only a prefix.
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if text, ok := token.(xml.CharData); ok && strings.TrimSpace(string(text)) == "" {
			continue
		}
		if _, ok := token.(xml.Comment); ok {
			continue
		}
		return nil, fmt.Errorf("OPML must contain one document")
	}
	sources := []roiSource{}
	var walk func([]Outline, string, int) error
	walk = func(outlines []Outline, folder string, depth int) error {
		if depth > 32 {
			return fmt.Errorf("OPML nesting exceeds 32")
		}
		for _, o := range outlines {
			title := o.Title
			if title == "" {
				title = o.Text
			}
			if o.URL == "" {
				next := title
				if folder != "" {
					next = folder + "/" + title
				}
				if err := walk(o.Children, next, depth+1); err != nil {
					return err
				}
				continue
			}
			entry := roiSource{Type: "rss", URL: o.URL, Title: title, SiteURL: o.Site}
			if folder != "" {
				entry.Folders = []string{folder}
			}
			if o.Type == "git" {
				entry = roiSource{Type: "git", Repository: o.URL, Title: title, Service: o.GitProvider, Mode: o.GitMode, IncludePreview: o.GitPreview, IconURL: o.GitIcon}
			}
			sources = append(sources, entry)
			if len(sources) > 10000 {
				return fmt.Errorf("import contains more than 10000 sources")
			}
		}
		return nil
	}
	if err = walk(file.Outlines, "", 0); err != nil {
		return nil, err
	}
	return sources, nil
}
func normalizeSubscription(entry roiSource) (importSubscription, error) {
	var source GitSource
	switch entry.Type {
	case "git":
		if err := validateTagFilter(entry.TagFilter); err != nil {
			return importSubscription{}, err
		}
		if entry.URL != "" || entry.SiteURL != "" || len(entry.Folders) != 0 || entry.UserAgent != "" {
			return importSubscription{}, fmt.Errorf("git sources cannot specify RSS fields")
		}
		if entry.Service == "auto" {
			return importSubscription{}, fmt.Errorf("git source must specify a detected service")
		}
		mode := entry.Mode
		if mode == "" {
			mode = "releases"
			if entry.Service == "gitweb" {
				mode = "tags"
			}
		}
		var err error
		source, err = parseGitSource(entry.Repository, entry.Service, mode, entry.IncludePreview)
		source.TagFilter = entry.TagFilter
		if err != nil {
			return importSubscription{}, err
		}
	case "rss":
		if err := validateUserAgent(entry.UserAgent); err != nil {
			return importSubscription{}, err
		}
		entry.UserAgent = strings.TrimSpace(entry.UserAgent)
		if entry.Repository != "" || entry.Service != "" || entry.Mode != "" || entry.IncludePreview || entry.TagFilter != "" {
			return importSubscription{}, fmt.Errorf("RSS sources cannot specify Git settings")
		}
		if err := validFeedURL(entry.URL); err != nil {
			return importSubscription{}, err
		}
		if entry.SiteURL != "" {
			if err := validFeedURL(entry.SiteURL); err != nil {
				return importSubscription{}, err
			}
		}
		for _, folder := range entry.Folders {
			if strings.TrimSpace(folder) == "" || !utf8.ValidString(folder) || utf8.RuneCountInString(folder) > 512 || strings.ContainsFunc(folder, unicode.IsControl) || reservedGitFolder(folder) {
				return importSubscription{}, fmt.Errorf("invalid RSS folder %q", folder)
			}
		}
		source.URL = entry.URL
	default:
		return importSubscription{}, fmt.Errorf("source must specify type rss or git")
	}
	if entry.IconURL != "" {
		if err := validFeedURL(entry.IconURL); err != nil {
			return importSubscription{}, err
		}
		source.Icon = entry.IconURL
	}
	entry.Title = strings.TrimSpace(entry.Title)
	if entry.Title == "" {
		entry.Title = source.URL
		if source.Provider != "" {
			entry.Title = source.title()
		}
	}
	if !utf8.ValidString(entry.Title) || utf8.RuneCountInString(entry.Title) > 512 || strings.ContainsFunc(entry.Title, unicode.IsControl) {
		return importSubscription{}, fmt.Errorf("invalid source title")
	}
	return importSubscription{entry, source}, nil
}
func (s *Store) importFile(r io.Reader, format string) error {
	entries, err := decodeSubscriptions(r, format)
	if err != nil {
		return err
	}
	subscriptions := make([]importSubscription, len(entries))
	for i, entry := range entries {
		subscriptions[i], err = normalizeSubscription(entry)
		if err != nil {
			return fmt.Errorf("source %d: %w", i+1, err)
		}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sub := range subscriptions {
		entry, source := sub.Entry, sub.Source
		exists, err := importURLExists(tx, source.URL)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		enabled := true
		if entry.Enabled != nil {
			enabled = *entry.Enabled
		}
		site := entry.SiteURL
		folders := entry.Folders
		if source.Provider != "" {
			site = source.URL
			folders = []string{gitFolder}
		}
		result, err := tx.Exec(`INSERT INTO feeds(url,title,site,active,paused,git_provider,git_repo,git_mode,git_preview,icon_url,user_agent,git_tag_filter) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(url) DO NOTHING`, source.URL, entry.Title, site, enabled, entry.Paused && enabled, source.Provider, source.Repo, source.Mode, source.Preview, source.Icon, entry.UserAgent, source.TagFilter)
		if err != nil {
			return err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if added == 0 {
			continue
		}
		for _, folder := range folders {
			if _, err = tx.Exec("INSERT OR IGNORE INTO folders(name) VALUES(?)", folder); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT OR IGNORE INTO feed_folders(feed_id,name) SELECT id,? FROM feeds WHERE url=?", folder, source.URL); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (s *Store) PreviewImport(r io.Reader, format string) (ImportPreview, error) {
	var preview ImportPreview
	entries, err := decodeSubscriptions(r, format)
	if err != nil {
		return preview, err
	}
	rows, err := s.DB.Query("SELECT url FROM feeds")
	if err != nil {
		return preview, err
	}
	seen := map[string]bool{}
	key := func(raw string) string { return raw[strings.IndexByte(raw, ':')+1:] }
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return preview, err
		}
		seen[key(raw)] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return preview, err
	}
	preview.Total = len(entries)
	preview.Items = []ImportItem{}
	for i, entry := range entries {
		item := ImportItem{Index: i + 1, Type: entry.Type, Title: entry.Title, URL: entry.URL}
		if entry.Type == "git" {
			item.URL = entry.Repository
		}
		normalized, err := normalizeSubscription(entry)
		if err != nil {
			item.Status = "invalid"
			item.Error = err.Error()
			preview.Invalid++
		} else {
			item.URL = normalized.Source.URL
			item.Title = normalized.Entry.Title
			if seen[key(item.URL)] {
				item.Status = "skip"
				preview.Skipped++
			} else {
				item.Status = "add"
				preview.Add++
				seen[key(item.URL)] = true
			}
		}
		preview.Items = append(preview.Items, item)
	}
	return preview, nil
}
