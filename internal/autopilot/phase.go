package autopilot

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// PhaseDocs is what Autopilot discovered about a roadmap phase by reading
// the repository's own authoritative documents -- never by hardcoding a
// phase's scope into Go source (per the V1 spec's "PHASE-SPECIFIC BEHAVIOR"
// and "CLAUDE INTEGRATION" sections).
type PhaseDocs struct {
	Phase         int
	RoadmapPath   string
	RoadmapExists bool
	PlanPath      string
	PlanExists    bool
	ADRPaths      []string // relative to RepoRoot, discovered by scanning PlanPath
}

// authoritativeDocs is the fixed set of always-relevant repository
// documents every generated prompt references, per the spec's "CLAUDE
// INTEGRATION" list (roadmap/testing/invariants/security/observability/
// compatibility). This list is deliberately small and stable -- it is not
// phase-specific, unlike PlanPath/ADRPaths above.
var authoritativeDocs = []string{
	"docs/enterprise-roadmap.md",
	"docs/testing-strategy.md",
	"docs/invariants.md",
	"docs/compatibility-policy.md",
	"docs/security-model.md",
	"docs/observability.md",
}

var adrRefPattern = regexp.MustCompile(`adr/(\d{4}-[a-z0-9-]+)\.md`)

// DiscoverPhase reads docs/phase-<N>-plan.md (if it exists) and scans it for
// ADR references, so prompt generation can point Claude at exactly the
// documents this phase's own planning pass already identified as
// authoritative, instead of Autopilot inventing or duplicating that scope.
func DiscoverPhase(repoRoot string, phase int) (PhaseDocs, error) {
	if phase <= 0 {
		return PhaseDocs{}, fmt.Errorf("phase must be a positive integer, got %d", phase)
	}
	d := PhaseDocs{
		Phase:       phase,
		RoadmapPath: "docs/enterprise-roadmap.md",
		PlanPath:    fmt.Sprintf("docs/phase-%d-plan.md", phase),
	}
	if _, err := os.Stat(filepath.Join(repoRoot, d.RoadmapPath)); err == nil {
		d.RoadmapExists = true
	}
	planAbs := filepath.Join(repoRoot, d.PlanPath)
	data, err := os.ReadFile(planAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return d, fmt.Errorf("reading %s: %w", d.PlanPath, err)
	}
	d.PlanExists = true
	seen := map[string]bool{}
	for _, m := range adrRefPattern.FindAllStringSubmatch(string(data), -1) {
		rel := "docs/adr/" + m[1] + ".md"
		if seen[rel] {
			continue
		}
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			seen[rel] = true
			d.ADRPaths = append(d.ADRPaths, rel)
		}
	}
	sort.Strings(d.ADRPaths)
	return d, nil
}

// AuthoritativeDocs returns the fixed, always-relevant document set,
// filtered to only those that exist in this checkout.
func AuthoritativeDocs(repoRoot string) []string {
	var out []string
	for _, p := range authoritativeDocs {
		if _, err := os.Stat(filepath.Join(repoRoot, p)); err == nil {
			out = append(out, p)
		}
	}
	return out
}
