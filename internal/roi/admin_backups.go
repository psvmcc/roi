package roi

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) backupStatistics(r *http.Request, panel *backupPanel) error {
	config := a.config.backupConfig()
	panel.Enabled, panel.Directory, panel.Time, panel.Keep = config.Enabled, config.Directory, config.Time, config.Keep
	if !config.Enabled {
		return nil
	}
	a.backupMu.Lock()
	panel.backupRuntime = a.backupState
	a.backupMu.Unlock()
	var err error
	panel.Files, panel.StoredBytes, err = backupFiles(r.Context(), config.Directory, a.config.Database)
	if err != nil {
		panel.ScanError = err.Error()
	}
	panel.StoredCount = int64(len(panel.Files))
	if panel.Next == 0 && !panel.Running {
		now := time.Now().UTC()
		next := backupTime(now, config.Time)
		if !now.Before(next) {
			exists := false
			for _, file := range panel.Files {
				if file.Name == "roi-"+now.Format("20060102")+".sqlite" {
					exists = true
					break
				}
			}
			if exists {
				next = backupTime(now.AddDate(0, 0, 1), config.Time)
			} else {
				next = now
			}
		}
		panel.Next = next.Unix()
	}
	// File listing is bounded; totals above always cover every matching file.
	if len(panel.Files) > 50 {
		panel.Files = panel.Files[:50]
	}
	var days int
	err = a.store.DB.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(attempts),0),COALESCE(SUM(created),0),COALESCE(SUM(bytes),0),COALESCE(SUM(errors),0),COALESCE(MIN(last_attempt),0),COALESCE(MAX(last_attempt),0),COALESCE(MAX(last_success),0),COUNT(*) FROM backup_days`).Scan(&panel.Attempts, &panel.Created, &panel.CreatedBytes, &panel.Errors, &panel.FirstAttempt, &panel.LastAttempt, &panel.LastSuccess, &days)
	if err != nil {
		return err
	}
	if days > 0 {
		if err = a.store.DB.QueryRowContext(r.Context(), "SELECT last_error FROM backup_days ORDER BY last_finished DESC,day DESC LIMIT 1").Scan(&panel.LastError); err != nil {
			return err
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("backup_page"))
	panel.Pages = max((days+29)/30, 1)
	panel.Page = min(max(page, 1), panel.Pages)
	link := func(page int) string {
		q := r.URL.Query()
		q.Del("fragment")
		q.Set("backup_page", strconv.Itoa(page))
		return "/maintenance?" + q.Encode() + "#backup-statistics"
	}
	if panel.Page > 1 {
		panel.PreviousURL = link(panel.Page - 1)
	}
	if panel.Page < panel.Pages {
		panel.NextURL = link(panel.Page + 1)
	}
	rows, err := a.store.DB.QueryContext(r.Context(), `SELECT day,attempts,created,bytes,errors,last_attempt,last_finished,last_success,duration_ms,last_error FROM backup_days ORDER BY day DESC LIMIT 30 OFFSET ?`, (panel.Page-1)*30)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var day backupDay
		if err := rows.Scan(&day.Day, &day.Attempts, &day.Created, &day.Bytes, &day.Errors, &day.LastAttempt, &day.LastFinished, &day.LastSuccess, &day.DurationMS, &day.LastError); err != nil {
			return err
		}
		day.Day = strings.ReplaceAll(day.Day, "-", ".")
		panel.Days = append(panel.Days, day)
	}
	return rows.Err()
}
