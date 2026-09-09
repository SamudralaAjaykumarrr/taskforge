package buildinfo

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestString verifies String() renders every field it documents -- Version,
// Commit, Date, and the Go runtime version/GOOS/GOARCH it was built
// with -- for both the package's zero-ldflags defaults and controlled
// release-style values, since -ldflags -X is invisible to `go test` and
// can only be exercised by setting the vars directly.
func TestString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		commit  string
		date    string
	}{
		{
			name:    "defaults",
			version: "dev",
			commit:  "none",
			date:    "unknown",
		},
		{
			name:    "release values",
			version: "v1.2.3",
			commit:  "abc123def456",
			date:    "2026-01-01T00:00:00Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origVersion, origCommit, origDate := Version, Commit, Date
			t.Cleanup(func() {
				Version, Commit, Date = origVersion, origCommit, origDate
			})
			Version, Commit, Date = tt.version, tt.commit, tt.date

			got := String()

			for _, want := range []string{
				tt.version,
				tt.commit,
				tt.date,
				runtime.Version(),
				runtime.GOOS,
				runtime.GOARCH,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("String() = %q, want it to contain %q", got, want)
				}
			}

			wantExact := fmt.Sprintf("%s (commit=%s date=%s go=%s %s/%s)",
				tt.version, tt.commit, tt.date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			if got != wantExact {
				t.Errorf("String() = %q, want %q", got, wantExact)
			}
		})
	}
}
