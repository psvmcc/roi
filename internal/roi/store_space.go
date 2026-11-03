package roi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func tombstoneDigest(guid string) []byte { sum := sha256.Sum256([]byte(guid)); return sum[:] }

// Exact fixed-size fingerprints preserve suppression without retaining arbitrary
// length GUIDs. Never expire them: doing so could resurrect removed articles.
func (s *Store) CompactTombstones(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT feed_id,guid FROM tombstones WHERE typeof(guid)='text' LIMIT 1000")
	if err != nil {
		return err
	}
	type key struct {
		id   int64
		guid string
	}
	keys := []key{}
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.id, &k.guid); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO tombstones(feed_id,guid) VALUES(?,?)", k.id, tombstoneDigest(k.guid)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM tombstones WHERE feed_id=? AND guid=?", k.id, k.guid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type SpaceUsage struct{ FreeBytes, Tombstones, TombstoneBytes int64 }

func (s *Store) Space(ctx context.Context) (SpaceUsage, error) {
	var usage SpaceUsage
	var pages, size int64
	if err := s.DB.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&pages); err != nil {
		return usage, err
	}
	if err := s.DB.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return usage, err
	}
	usage.FreeBytes = pages * size
	err := s.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(tombstones),0),COALESCE(SUM(key_bytes),0) FROM feed_stats").Scan(&usage.Tombstones, &usage.TombstoneBytes)
	return usage, err
}

// CompactDatabase is deliberately offline. It takes the same instance lock as
// the server, checks disk headroom and makes a verified backup before VACUUM.
func CompactDatabase(ctx context.Context, c Config) (string, error) {
	if err := checkStoragePermissions(c); err != nil {
		return "", err
	}
	lock, err := lockInstance(c.Database)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	s, err := OpenStore(c.Database)
	if err != nil {
		return "", err
	}
	defer s.DB.Close()
	info, err := os.Stat(c.Database)
	if err != nil {
		return "", err
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(filepath.Dir(c.Database), &fs); err != nil {
		return "", err
	}
	required := uint64(info.Size())*3 + (16 << 20)
	if uint64(fs.Bavail)*uint64(fs.Bsize) < required {
		return "", fmt.Errorf("database compaction requires at least %d bytes of free disk space for backup and VACUUM", required)
	}
	backup := c.Database + ".before-compact-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".sqlite"
	if err = writeBackup(ctx, c.Database, filepath.Dir(c.Database), backup); err != nil {
		return "", err
	}
	if _, err = s.DB.ExecContext(ctx, "VACUUM"); err != nil {
		return backup, err
	}
	if _, err = s.Checkpoint(ctx, true); err != nil {
		return backup, err
	}
	return backup, checkBackup(ctx, c.Database)
}
