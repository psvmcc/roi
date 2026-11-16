package roi

import (
	"database/sql"
	"strings"
)

// Persist match results so SQLite queries and counter triggers need no regex
// extension. Changes are atomic with the subscription settings and keep IDs.
func updateRegexTagMatches(tx *sql.Tx, id int64, pattern, mode string) error {
	if mode != "regex" || pattern == "" {
		_, err := tx.Exec("UPDATE entries SET git_tag_match=1 WHERE feed_id=? AND git_tag_match<>1", id)
		return err
	}
	re, err := compileTagFilter(pattern, mode)
	if err != nil {
		return err
	}
	rows, err := tx.Query("SELECT id,guid,git_tag_match FROM entries WHERE feed_id=?", id)
	if err != nil {
		return err
	}
	type change struct {
		id    int64
		match bool
	}
	var changes []change
	for rows.Next() {
		var entryID int64
		var guid string
		var previous bool
		if err = rows.Scan(&entryID, &guid, &previous); err != nil {
			rows.Close()
			return err
		}
		match := re.MatchString(strings.TrimPrefix(guid, "git:"))
		if previous != match {
			changes = append(changes, change{entryID, match})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range changes {
		if _, err = tx.Exec("UPDATE entries SET git_tag_match=? WHERE id=?", c.match, c.id); err != nil {
			return err
		}
	}
	return nil
}
