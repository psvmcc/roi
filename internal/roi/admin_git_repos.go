package roi

import (
	"net/http"
	"strconv"
	"strings"
)

func (a *App) adminGitRepos(w http.ResponseWriter, r *http.Request) bool {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil || id < 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return false
	}
	raw, provider, ownerType := r.FormValue("account"), r.FormValue("provider"), r.FormValue("owner_type")
	var ownerID int64
	var icon string
	if id > 0 {
		err = a.store.DB.QueryRow("SELECT url,git_provider,git_owner_type,git_owner_id,icon_url FROM feeds WHERE id=? AND source_kind='git_repos'", id).Scan(&raw, &provider, &ownerType, &ownerID, &icon)
		if err != nil {
			a.adminStoreError(w, r, ErrFeedNotFound)
			return false
		}
	}
	var override *string
	if r.Form.Has("icon_override") {
		value := strings.TrimSpace(r.FormValue("icon_override"))
		if err = validateIconOverride(value); err != nil {
			a.adminStoreError(w, r, err)
			return false
		}
		override = &value
	}
	source, err := parseGitReposSource(raw, provider, ownerType, r.FormValue("include_subgroups") == "1")
	if err == nil && id == 0 {
		source, err = a.poller.discoverGitRepos(r.Context(), source)
	}
	if err != nil {
		a.adminStoreError(w, r, clientError(err.Error()))
		return false
	}
	source.IgnoreForks = r.FormValue("ignore_forks") == "1"
	if id > 0 {
		source.OwnerID, source.Icon = ownerID, icon
		a.poller.Cancel(id)
	}
	if err = a.store.SaveGitReposSource(id, source, override); err != nil {
		a.adminStoreError(w, r, err)
		return false
	}
	a.poller.Wake()
	return true
}
