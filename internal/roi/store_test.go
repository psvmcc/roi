package roi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelativeDatabaseAndSchemaGuard(t *testing.T) {
	cwd, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "nested", "relative.db")
	relative, e := filepath.Rel(cwd, path)
	if e != nil {
		t.Fatal(e)
	}
	s, e := OpenStore(relative)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("PRAGMA user_version=999"); e != nil {
		t.Fatal(e)
	}
	if e = s.DB.Close(); e != nil {
		t.Fatal(e)
	}
	if s, e = OpenStore(relative); e == nil {
		s.DB.Close()
		t.Fatal("opened unsupported schema")
	}
}

func TestSQLitePragmas(t *testing.T) {
	a := testApp(t)
	for pragma, want := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "5000", "wal_autocheckpoint": "1000", "journal_size_limit": "4194304"} {
		var got string
		if e := a.store.DB.QueryRow("PRAGMA " + pragma).Scan(&got); e != nil {
			t.Fatal(e)
		}
		if got != want {
			t.Fatalf("%s=%s want %s", pragma, got, want)
		}
	}
}
