package roi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func RotateStoppedAPIKey(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := checkStoragePermissions(c); err != nil {
		return err
	}
	lock, err := lockInstance(c.Database)
	if err != nil {
		return err
	}
	defer lock.Close()
	s, err := OpenStore(c.Database)
	if err != nil {
		return err
	}
	defer s.DB.Close()
	_, err = s.RotateSigningKey()
	return err
}

// Restore is offline and stages a complete, migrated, session-revoked database
// before changing the destination. The previous DB and WAL/SHM are retained in
// a private directory, including when the old database is corrupt.
func RestoreDatabase(ctx context.Context, c Config, source string) (backup string, result error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if err := checkPrivateFile(source, c.security().FilePermissions); err != nil {
		return "", err
	}
	if err := checkStoragePermissions(c); err != nil {
		return "", err
	}
	lock, err := lockInstance(c.Database)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	target, err := filepath.EvalSymlinks(c.Database)
	if err != nil {
		return "", err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if os.SameFile(sourceInfo, targetInfo) {
		return "", fmt.Errorf("restore source must differ from the destination database")
	}
	stage, err := os.CreateTemp(filepath.Dir(target), ".roi-restore-*.sqlite")
	if err != nil {
		return "", err
	}
	name := stage.Name()
	if err = stage.Close(); err != nil {
		return "", err
	}
	if err = os.Remove(name); err != nil {
		return "", err
	}
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(name + suffix)
		}
	}()
	if err = writeBackup(ctx, source, filepath.Dir(target), name); err != nil {
		return "", err
	}
	s, err := OpenStore(name)
	if err != nil {
		return "", err
	}
	if _, err = s.RotateSigningKey(); err == nil {
		_, err = s.DB.ExecContext(ctx, "UPDATE auth_sessions SET revoked=1")
	}
	if err == nil {
		_, err = s.DB.ExecContext(ctx, "DELETE FROM git_http_cache")
	}
	if err == nil {
		_, err = s.Checkpoint(ctx, true)
	}
	if err == nil {
		var mode string
		err = s.DB.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode)
	}
	err = errors.Join(err, s.DB.Close())
	if err != nil {
		return "", err
	}
	if err = checkBackup(ctx, name); err != nil {
		return "", err
	}
	backup, err = os.MkdirTemp(filepath.Dir(target), "roi-before-restore-")
	if err != nil {
		return "", err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		err = copyPrivateFile(ctx, target+suffix, filepath.Join(backup, "database.sqlite"+suffix))
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return backup, err
		}
	}
	if err = syncDirectory(backup); err != nil {
		return backup, err
	}
	moved := []string{}
	published := false
	defer func() {
		if !published {
			for _, suffix := range moved {
				result = errors.Join(result, os.Rename(filepath.Join(backup, "moved"+suffix), target+suffix))
			}
		}
	}()
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = os.Rename(target+suffix, filepath.Join(backup, "moved"+suffix)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return backup, err
		}
		moved = append(moved, suffix)
	}
	if err = ctx.Err(); err != nil {
		return backup, err
	}
	if err = os.Rename(name, target); err != nil {
		return backup, err
	}
	published = true
	return backup, syncDirectory(filepath.Dir(target))
}
func copyPrivateFile(ctx context.Context, source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("restore backup source must be a regular file")
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	buffer := make([]byte, 128<<10)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			if _, err = out.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return out.Sync()
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
