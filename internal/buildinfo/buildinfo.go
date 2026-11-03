// Package buildinfo identifies the executable without depending on application code.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set by the build script using -ldflags -X.
var Version = "dev"
var Commit = "unknown"
var Date = "unknown"

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			Version = info.Main.Version
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && Commit == "unknown" {
				Commit = s.Value
			}
		}
	}
}
func UserAgent() string { return "roi/" + Version }
func Label() string {
	if Commit == "unknown" {
		return Version
	}
	return Version + " (" + Commit[:min(len(Commit), 12)] + ")"
}
func String() string {
	return fmt.Sprintf("roi %s\ncommit: %s\nbuilt: %s\ngo: %s", Version, Commit, Date, runtime.Version())
}
