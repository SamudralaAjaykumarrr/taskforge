package autopilot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ManifestRelDir is the checked-in (never gitignored) directory holding one
// phase proof manifest per roadmap phase, e.g. autopilot/phases/16.json.
// This is deliberately NOT under .taskforge-autopilot/ (that directory is
// gitignored, run-local state, per docs/autopilot.md) -- a manifest is
// authoritative, reviewed, committed content, binding Autopilot's
// local-gates execution to what a phase's actual approved plan requires.
const ManifestRelDir = "autopilot/phases"

// ProofType distinguishes a proof obligation Autopilot can safely execute
// itself from one that cannot be safely automated and must instead be
// explicitly approved by a human.
type ProofType string

const (
	ProofCommand ProofType = "command"
	ProofHuman   ProofType = "human"
)

// Proof is one phase-specific proof obligation beyond the fixed core gates
// (gofmt/vet/build/test/test-race/mod-verify/diff-check).
type Proof struct {
	Name        string    `json:"name"`
	Type        ProofType `json:"type"`
	Command     []string  `json:"command,omitempty"`     // required, non-empty, when Type==ProofCommand
	Description string    `json:"description,omitempty"` // required, non-empty, when Type==ProofHuman
	Required    bool      `json:"required"`
}

// PhaseManifest is one phase's checked-in, machine-readable proof
// obligation registry. It binds itself to the authoritative phase plan by
// path and content digest, so it can never silently drift out of sync with
// what the plan actually requires.
type PhaseManifest struct {
	Phase    int    `json:"phase"`
	PlanPath string `json:"plan_path"`
	// PlanSHA256 is the SHA-256 hex digest of PlanPath's exact bytes at the
	// time this manifest was authored/reviewed. A mismatch means the plan
	// changed since the manifest was last reconciled -- see LoadManifest.
	PlanSHA256 string `json:"plan_sha256"`
	// NoExtraProofsReviewed must be true when Proofs is empty, as an
	// explicit statement that "this phase has no proof obligations beyond
	// the fixed core gates" was a reviewed decision, not an omission. An
	// empty Proofs with this false (or absent) is treated as malformed --
	// nil/empty must never silently mean "nothing to prove."
	NoExtraProofsReviewed bool    `json:"no_extra_proofs_reviewed,omitempty"`
	Proofs                []Proof `json:"proofs"`
}

// ManifestPath returns the checked-in manifest path for a phase.
func ManifestPath(repoRoot string, phase int) string {
	return filepath.Join(repoRoot, ManifestRelDir, fmt.Sprintf("%d.json", phase))
}

// ComputeFileDigestSHA256 returns the lowercase-hex SHA-256 digest of a
// file's exact current bytes.
func ComputeFileDigestSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// LoadManifest reads, parses, and fully validates a phase's proof manifest
// against the actual authoritative plan file on disk. It fails closed --
// returns a non-nil error, never a best-effort partial manifest -- for
// every one of: a missing file, malformed JSON, a phase-number mismatch, a
// plan-path mismatch, a plan-digest mismatch (the plan changed since the
// manifest was authored), an incomplete/invalid proof entry, or an empty
// Proofs list that was not explicitly marked reviewed
// (NoExtraProofsReviewed). Callers (doLocalGates, dry-run, prompt
// generation) must never treat a LoadManifest error as "no proofs" --
// it means the manifest cannot currently be trusted at all.
func LoadManifest(repoRoot string, phase int, expectedPlanPath string) (*PhaseManifest, error) {
	path := ManifestPath(repoRoot, phase)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("phase %d proof manifest %s does not exist", phase, path)
		}
		return nil, fmt.Errorf("reading phase %d proof manifest %s: %w", phase, path, err)
	}
	var m PhaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("phase %d proof manifest %s is malformed JSON: %w", phase, path, err)
	}
	if m.Phase != phase {
		return nil, fmt.Errorf("phase %d proof manifest %s declares phase=%d, expected %d", phase, path, m.Phase, phase)
	}
	if m.PlanPath != expectedPlanPath {
		return nil, fmt.Errorf("phase %d proof manifest %s declares plan_path=%q, expected %q", phase, path, m.PlanPath, expectedPlanPath)
	}
	if m.PlanSHA256 == "" {
		return nil, fmt.Errorf("phase %d proof manifest %s has no plan_sha256", phase, path)
	}
	actualDigest, err := ComputeFileDigestSHA256(filepath.Join(repoRoot, m.PlanPath))
	if err != nil {
		return nil, fmt.Errorf("computing digest of %s to verify manifest %s: %w", m.PlanPath, path, err)
	}
	if actualDigest != m.PlanSHA256 {
		return nil, fmt.Errorf("phase %d proof manifest %s is stale: plan_sha256=%s but %s currently hashes to %s -- the authoritative plan changed since this manifest was reviewed; reconcile the manifest before continuing", phase, path, m.PlanSHA256, m.PlanPath, actualDigest)
	}

	if len(m.Proofs) == 0 && !m.NoExtraProofsReviewed {
		return nil, fmt.Errorf("phase %d proof manifest %s declares zero proofs without an explicit no_extra_proofs_reviewed=true confirmation -- an empty/absent proof list is never silently treated as \"nothing to prove\"", phase, path)
	}

	seen := map[string]bool{}
	for i, p := range m.Proofs {
		if p.Name == "" {
			return nil, fmt.Errorf("phase %d proof manifest %s: proof at index %d has no name", phase, path, i)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("phase %d proof manifest %s: duplicate proof name %q", phase, path, p.Name)
		}
		seen[p.Name] = true
		switch p.Type {
		case ProofCommand:
			if len(p.Command) == 0 {
				return nil, fmt.Errorf("phase %d proof manifest %s: command proof %q has an empty command", phase, path, p.Name)
			}
		case ProofHuman:
			if p.Description == "" {
				return nil, fmt.Errorf("phase %d proof manifest %s: human proof %q has no description", phase, path, p.Name)
			}
		default:
			return nil, fmt.Errorf("phase %d proof manifest %s: proof %q has unrecognized type %q (must be %q or %q)", phase, path, p.Name, p.Type, ProofCommand, ProofHuman)
		}
	}

	return &m, nil
}

// RequiredCommandProofs returns the manifest's required, automatable proof
// obligations as gateSpecs, ready to append to the fixed core gate list.
func (m *PhaseManifest) RequiredCommandProofs() []gateSpec {
	var extra []gateSpec
	for _, p := range m.Proofs {
		if p.Type == ProofCommand && p.Required {
			extra = append(extra, gateSpec{Name: "phase-proof: " + p.Name, Args: p.Command})
		}
	}
	return extra
}

// RequiredHumanProofs returns the manifest's required, non-automatable
// proof obligations.
func (m *PhaseManifest) RequiredHumanProofs() []Proof {
	var out []Proof
	for _, p := range m.Proofs {
		if p.Type == ProofHuman && p.Required {
			out = append(out, p)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
