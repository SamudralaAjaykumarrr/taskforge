// Package buildinfo holds TaskForge's release version metadata. Version,
// Commit, and Date are the package's only mutable state and are set once, at
// build time, via linker flags (-ldflags "-X ...") from
// .github/workflows/release.yml / `make release-build` — never written to at
// runtime. This is the single place version metadata lives; cmd/api and
// cmd/worker import it rather than declaring their own version constants.
package buildinfo

import (
	"fmt"
	"runtime"
)

// Version, Commit, and Date default to "dev"/"none"/"unknown" for a plain
// `go build` or `go run` with no ldflags, which is expected and correct for
// local development builds — only release builds inject real values.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders a single-line, human-readable version string suitable for
// a -version flag or a startup log line.
func String() string {
	return fmt.Sprintf("%s (commit=%s date=%s go=%s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
