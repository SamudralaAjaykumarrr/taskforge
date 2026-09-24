// Command taskforge-autopilot is repository/developer tooling that carries
// a TaskForge roadmap phase through this project's real planning ->
// implementation -> review -> CI -> merge workflow, pausing at explicit
// human-approval boundaries. It is not part of the TaskForge job-processing
// runtime. See docs/autopilot.md.
package main

import (
	"os"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/autopilot"
)

func main() {
	os.Exit(autopilot.Run(os.Args[1:], os.Stdout, os.Stderr))
}
