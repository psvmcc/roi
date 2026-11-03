package roi

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := checkPrivateFile(path, "warn"); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = absolute
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "wal_autocheckpoint(1000)")
	q.Add("_pragma", "journal_size_limit(4194304)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db}, nil
}

// Checkpoint copies committed WAL frames into the database. When idle, it also
// releases the WAL file's allocated space. External readers never make it wait.
// A false result means a reader held a snapshot; maintenance retries later.
func (s *Store) Checkpoint(ctx context.Context, idle bool) (bool, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return false, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000") }()
	mode := "PASSIVE"
	if idle {
		mode = "TRUNCATE"
	}
	var busy, frames, copied int
	err = conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &frames, &copied)
	return busy == 0 && frames == copied, err
}
