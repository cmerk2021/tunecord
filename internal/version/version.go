// Package version holds build information injected via -ldflags.
package version

import (
	"fmt"
	"runtime/debug"
)

// Set by GoReleaser: -X tunecord/internal/version.Version=... etc.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// RepoURL is the project homepage, used in the MusicBrainz User-Agent.
const RepoURL = "https://github.com/cmerk2021/tunecore"

// Get returns the version, falling back to module build info for
// `go install` builds.
func Get() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}

// UserAgent returns the HTTP User-Agent for API requests.
func UserAgent() string {
	return fmt.Sprintf("tunecord/%s (%s)", Get(), RepoURL)
}

// String returns a human-readable version line.
func String() string {
	return fmt.Sprintf("tunecord %s (commit %s, built %s)", Get(), Commit, Date)
}
