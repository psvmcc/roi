package roi

import (
	"context"
	"io"

	"go.yaml.in/yaml/v3"
)

const maxROIBytes = 8 << 20

// ROI is a versioned, human-editable subscription format. It deliberately
// carries source settings only, never articles, sessions or API credentials.
type roiFile struct {
	Format  string      `yaml:"format"`
	Version int         `yaml:"version"`
	Sources []roiSource `yaml:"sources"`
}
type roiSource struct {
	UserAgent      string   `yaml:"user_agent,omitempty"`
	Type           string   `yaml:"type,omitempty"`
	URL            string   `yaml:"url,omitempty"`
	SiteURL        string   `yaml:"site_url,omitempty"`
	SiteOverride   string   `yaml:"site_url_override,omitempty"`
	Folders        []string `yaml:"folders,omitempty"`
	Repository     string   `yaml:"repository,omitempty"`
	Service        string   `yaml:"service,omitempty"`
	Title          string   `yaml:"title,omitempty"`
	Mode           string   `yaml:"mode,omitempty"`
	TagFilter      string   `yaml:"tag_filter,omitempty"`
	IncludePreview bool     `yaml:"include_preview,omitempty"`
	Enabled        *bool    `yaml:"enabled,omitempty"`
	Paused         bool     `yaml:"paused"`
	IconURL        string   `yaml:"icon_url,omitempty"`
	IconOverride   string   `yaml:"icon_url_override,omitempty"`
}

func (s *Store) ExportROI(w io.Writer) error {
	return s.exportROI(context.Background(), w)
}

func (s *Store) exportROI(ctx context.Context, w io.Writer) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,url,title,CASE WHEN site_auto<>'' THEN site_auto WHEN site_override='' THEN site ELSE '' END,active,paused,git_provider,git_mode,git_preview,icon_url,user_agent,git_tag_filter,site_override,icon_override FROM feeds ORDER BY url`)
	if err != nil {
		return err
	}
	file := roiFile{Format: "roi", Version: 1, Sources: []roiSource{}}
	positions := map[int64]int{}
	for rows.Next() {
		var id int64
		var provider string
		var enabled bool
		var entry roiSource
		if err = rows.Scan(&id, &entry.URL, &entry.Title, &entry.SiteURL, &enabled, &entry.Paused, &provider, &entry.Mode, &entry.IncludePreview, &entry.IconURL, &entry.UserAgent, &entry.TagFilter, &entry.SiteOverride, &entry.IconOverride); err != nil {
			rows.Close()
			return err
		}
		entry.Enabled = &enabled
		entry.Type = "rss"
		if provider == "" {
			entry.Mode = ""
			entry.TagFilter = ""
			entry.IncludePreview = false
		}
		if provider != "" {
			entry.Type, entry.Repository, entry.Service, entry.URL, entry.SiteURL = "git", entry.URL, provider, "", ""
		}
		positions[id] = len(file.Sources)
		file.Sources = append(file.Sources, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	folders, err := tx.QueryContext(ctx, "SELECT feed_id,name FROM feed_folders ORDER BY feed_id,name")
	if err != nil {
		return err
	}
	for folders.Next() {
		var id int64
		var name string
		if err = folders.Scan(&id, &name); err != nil {
			folders.Close()
			return err
		}
		if index, ok := positions[id]; ok && file.Sources[index].Type == "rss" {
			file.Sources[index].Folders = append(file.Sources[index].Folders, name)
		}
	}
	err = folders.Err()
	folders.Close()
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if _, err = io.WriteString(w, "# ROI subscriptions (.roi). No credentials or article history.\n"); err != nil {
		return err
	}
	encoder := yaml.NewEncoder(w)
	encoder.SetIndent(2)
	defer encoder.Close()
	return encoder.Encode(file)
}
func (s *Store) ImportROI(r io.Reader) error { return s.importFile(r, "roi") }
