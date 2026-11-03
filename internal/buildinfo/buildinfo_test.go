package buildinfo

import (
	"strings"
	"testing"
)

func TestBuildMetadata(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	defer func() { Version, Commit, Date = oldVersion, oldCommit, oldDate }()
	Version = "v1.2.3"
	Commit = "1234567890abcdef"
	Date = "2026-11-02T12:00:00Z"
	if UserAgent() != "roi/v1.2.3" || Label() != "v1.2.3 (1234567890ab)" {
		t.Fatal(UserAgent(), Label())
	}
	for _, part := range []string{"roi v1.2.3", "commit: " + Commit, "built: " + Date, "go: go"} {
		if !strings.Contains(String(), part) {
			t.Fatal(String())
		}
	}
	Commit = "short"
	if Label() != "v1.2.3 (short)" {
		t.Fatal(Label())
	}
	Commit = "unknown"
	if Label() != "v1.2.3" {
		t.Fatal(Label())
	}
}
