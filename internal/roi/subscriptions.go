package roi

import (
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
}

func (s *Store) ExportROI(w io.Writer) error {
	feeds, err := s.Feeds()
	if err != nil {
		return err
	}
	file := roiFile{Format: "roi", Version: 1, Sources: []roiSource{}}
	for _, f := range feeds {
		enabled := f.Active
		entry := roiSource{Type: "rss", URL: f.URL, SiteURL: f.Site, Folders: f.Folders, Title: f.Title, Enabled: &enabled, Paused: f.Paused, IconURL: f.Icon, UserAgent: f.UserAgent}
		if f.GitProvider != "" {
			entry.TagFilter = f.GitTagFilter
			entry.Type, entry.URL, entry.SiteURL, entry.Folders = "git", "", "", nil
			entry.Repository, entry.Service, entry.Mode, entry.IncludePreview = f.URL, f.GitProvider, f.GitMode, f.GitPreview
		}
		file.Sources = append(file.Sources, entry)
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
