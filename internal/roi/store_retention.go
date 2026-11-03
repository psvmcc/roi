package roi

import (
	"strings"
	"time"
)

func (s *Store) Rotate(c Config, now time.Time) (int64, error) {
	if c.Retention.Months == 0 && c.Retention.PerFeed == 0 {
		return 0, nil
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	cutoff := int64(0)
	if c.Retention.Months > 0 {
		cutoff = now.UTC().AddDate(0, -c.Retention.Months, 0).UnixMicro()
	}
	keep := 0
	if c.Retention.KeepStarred {
		keep = 1
	}
	// At most 1000 entries per transaction. A minute-by-minute maintenance pass drains large backlogs.
	where := `WITH ranked AS (SELECT id,feed_id,guid,received,starred,ROW_NUMBER() OVER(PARTITION BY feed_id ORDER BY received DESC,id DESC) AS rn FROM entries) SELECT id FROM ranked WHERE (?=0 OR starred=0) AND ((?>0 AND received<?) OR (?>0 AND rn>?)) LIMIT 1000`
	args := []any{keep, cutoff, cutoff, c.Retention.PerFeed, c.Retention.PerFeed}
	rows, e := tx.Query("SELECT id,feed_id,guid FROM entries WHERE id IN ("+where+")", args...)
	if e != nil {
		return 0, e
	}
	type retired struct {
		id, feed int64
		digest   []byte
	}
	items := []retired{}
	for rows.Next() {
		var item retired
		var guid string
		if e = rows.Scan(&item.id, &item.feed, &guid); e != nil {
			rows.Close()
			return 0, e
		}
		item.digest = tombstoneDigest(guid)
		items = append(items, item)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, e
	}
	ids := []any{}
	for _, item := range items {
		if _, e = tx.Exec("INSERT OR IGNORE INTO tombstones(feed_id,guid) VALUES(?,?)", item.feed, item.digest); e != nil {
			return 0, e
		}
		ids = append(ids, item.id)
	}
	var n int64
	if len(ids) > 0 {
		res, err := tx.Exec("DELETE FROM entries WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", ids...)
		if err != nil {
			return 0, err
		}
		n, e = res.RowsAffected()
		if e != nil {
			return 0, e
		}
	}
	if _, e = tx.Exec("INSERT INTO maintenance VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET last_run=excluded.last_run,deleted=excluded.deleted", now.Unix(), n); e != nil {
		return 0, e
	}
	return n, tx.Commit()
}
