package roi

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock a sidecar next to the canonical database path. Locking the database
// itself would interfere with SQLite file locks on macOS.
func lockInstance(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	database, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = database.Close(); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(canonical+".roi.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("database %q is already used by another roi instance (or cannot be locked): %w", path, err)
	}
	return file, nil
}
