package roi

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

func checkPrivateFile(path, policy string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("private file %q must be a regular file", path)
	}
	if info.Mode().Perm()&0077 == 0 {
		return nil
	}
	if policy == "strict" {
		return fmt.Errorf("private file %q has permissions %04o; remove group/other access", path, info.Mode().Perm())
	}
	slog.Warn("private file is accessible to other users", "path", path, "permissions", fmt.Sprintf("%04o", info.Mode().Perm()))
	return nil
}
func checkStoragePermissions(c Config) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := checkPrivateFile(c.Database+suffix, c.security().FilePermissions); err != nil {
			return err
		}
	}
	dir := filepath.Dir(c.Database)
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("database directory %q must be a directory", dir)
	}
	if info.Mode().Perm()&0077 != 0 {
		if c.security().FilePermissions == "strict" {
			return fmt.Errorf("database directory %q must not allow group/other access", dir)
		}
		slog.Warn("database directory is accessible to other users", "path", dir)
	}
	return nil
}
