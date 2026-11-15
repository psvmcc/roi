package roi

import "strings"

func (s *Store) SaveGitReposSource(id int64, g GitSource, iconOverride *string) error {
	checked, err := parseGitReposSource(g.URL, g.Provider, g.OwnerType, g.IncludeSubgroups)
	if err != nil {
		return err
	}
	if checked.Provider == "auto" {
		return clientError("detect the Git service before saving")
	}
	var icon string
	if iconOverride != nil {
		icon = strings.TrimSpace(*iconOverride)
		if err = validateIconOverride(icon); err != nil {
			return err
		}
	}
	if g.Icon != "" {
		if err = validFeedURL(g.Icon); err != nil {
			return clientError(err.Error())
		}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if id == 0 {
		exists, err := importURLExists(tx, checked.URL)
		if err != nil {
			return err
		}
		if exists {
			return clientError("account is already subscribed; edit the existing source")
		}
		result, err := tx.Exec(`INSERT INTO feeds(url,title,site,git_provider,git_repo,icon_url,source_kind,git_owner_type,git_owner_id,git_include_subgroups,icon_override) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, checked.URL, checked.title(), checked.URL, checked.Provider, checked.Repo, g.Icon, kindGitRepos, checked.OwnerType, g.OwnerID, checked.IncludeSubgroups, icon)
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
	} else {
		result, err := tx.Exec(`UPDATE feeds SET git_include_subgroups=?,icon_override=CASE WHEN ? THEN ? ELSE icon_override END,git_revision=git_revision+1,next_fetch=0,error='',failures=0 WHERE id=? AND source_kind='git_repos' AND url=? AND git_provider=?`, checked.IncludeSubgroups, iconOverride != nil, icon, id, checked.URL, checked.Provider)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return clientError("unknown Git account or account URL changed")
		}
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO folders(name) VALUES(?)", gitReposFolder); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO feed_folders(feed_id,name) VALUES(?,?)", id, gitReposFolder); err != nil {
		return err
	}
	return tx.Commit()
}
