package autopilot

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	resultBeginMarker = "AUTOPILOT_RESULT_BEGIN"
	resultEndMarker   = "AUTOPILOT_RESULT_END"
)

// parseResultBlock extracts the last AUTOPILOT_RESULT_BEGIN/...END block in
// output and returns its KEY=VALUE pairs. It fails closed: any of "no
// block found", "unterminated block", "malformed line" is a hard error, not
// a best-effort partial parse -- Autopilot must never guess at a missing or
// malformed machine-readable contract (see docs/autopilot.md "Claude Result
// Contract").
func parseResultBlock(output string) (map[string]string, error) {
	beginIdx := strings.LastIndex(output, resultBeginMarker)
	if beginIdx < 0 {
		return nil, fmt.Errorf("no %s block found in Claude output -- failing closed rather than guessing at intent", resultBeginMarker)
	}
	afterBegin := output[beginIdx+len(resultBeginMarker):]
	endIdx := strings.Index(afterBegin, resultEndMarker)
	if endIdx < 0 {
		return nil, fmt.Errorf("%s found but %s is missing -- unterminated result block, failing closed", resultBeginMarker, resultEndMarker)
	}
	body := strings.TrimSpace(afterBegin[:endIdx])
	fields := map[string]string{}
	if body == "" {
		return nil, fmt.Errorf("result block is empty")
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			return nil, fmt.Errorf("malformed result line %q: expected KEY=VALUE", line)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		fields[key] = val
	}
	return fields, nil
}

func requireField(fields map[string]string, key string) (string, error) {
	v, ok := fields[key]
	if !ok || v == "" {
		return "", fmt.Errorf("result block missing required field %q", key)
	}
	return v, nil
}

// ReviewResult is the parsed, validated outcome of an independent review
// (planning or implementation).
type ReviewResult struct {
	Verdict      string // APPROVE or BLOCKED
	Ready        bool
	BlockerCount int
	BlockerType  BlockerType
	Blockers     string
	Raw          string
}

// ParseReviewResult parses and validates a reviewer's result contract.
// Fails closed on any missing/malformed/inconsistent field.
func ParseReviewResult(output string) (ReviewResult, error) {
	fields, err := parseResultBlock(output)
	if err != nil {
		return ReviewResult{}, err
	}
	verdict, err := requireField(fields, "VERDICT")
	if err != nil {
		return ReviewResult{}, err
	}
	verdict = strings.ToUpper(verdict)
	if verdict != "APPROVE" && verdict != "BLOCKED" {
		return ReviewResult{}, fmt.Errorf("result field VERDICT=%q is not APPROVE or BLOCKED", verdict)
	}
	readyStr, err := requireField(fields, "READY")
	if err != nil {
		return ReviewResult{}, err
	}
	ready, err := strconv.ParseBool(readyStr)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("result field READY=%q is not a boolean", readyStr)
	}
	countStr, err := requireField(fields, "BLOCKER_COUNT")
	if err != nil {
		return ReviewResult{}, err
	}
	count, err := strconv.Atoi(countStr)
	if err != nil || count < 0 {
		return ReviewResult{}, fmt.Errorf("result field BLOCKER_COUNT=%q is not a non-negative integer", countStr)
	}
	if verdict == "APPROVE" && count != 0 {
		return ReviewResult{}, fmt.Errorf("inconsistent result: VERDICT=APPROVE but BLOCKER_COUNT=%d", count)
	}
	if verdict == "BLOCKED" && count == 0 {
		return ReviewResult{}, fmt.Errorf("inconsistent result: VERDICT=BLOCKED but BLOCKER_COUNT=0")
	}
	if verdict == "APPROVE" && !ready {
		return ReviewResult{}, fmt.Errorf("inconsistent result: VERDICT=APPROVE but READY=false")
	}
	blockerType := BlockerNone
	if verdict == "BLOCKED" {
		bt, err := requireField(fields, "BLOCKER_TYPE")
		if err != nil {
			return ReviewResult{}, fmt.Errorf("VERDICT=BLOCKED requires BLOCKER_TYPE to be classified: %w", err)
		}
		blockerType = BlockerType(strings.ToLower(bt))
		switch blockerType {
		case BlockerOrdinary, BlockerArchitecture, BlockerMigration, BlockerSecurity, BlockerInvariant, BlockerProofWeaken:
			// valid
		default:
			return ReviewResult{}, fmt.Errorf("result field BLOCKER_TYPE=%q is not a recognized category", bt)
		}
	}
	return ReviewResult{
		Verdict:      verdict,
		Ready:        ready,
		BlockerCount: count,
		BlockerType:  blockerType,
		Blockers:     fields["BLOCKERS"],
		Raw:          output,
	}, nil
}

// ImplementationResult is the parsed, validated outcome of the
// implementation (or fix) agent's own self-report. Autopilot treats this as
// a claim to be verified by local gates, never as a substitute for them.
type ImplementationResult struct {
	Validation     string // PASS or FAIL
	ReadyForReview bool
	BlockerCount   int
	Blockers       string
	Raw            string
}

// ParseImplementationResult parses and validates the implementation
// contract.
func ParseImplementationResult(output string) (ImplementationResult, error) {
	fields, err := parseResultBlock(output)
	if err != nil {
		return ImplementationResult{}, err
	}
	validation, err := requireField(fields, "VALIDATION")
	if err != nil {
		return ImplementationResult{}, err
	}
	validation = strings.ToUpper(validation)
	if validation != "PASS" && validation != "FAIL" {
		return ImplementationResult{}, fmt.Errorf("result field VALIDATION=%q is not PASS or FAIL", validation)
	}
	readyStr, err := requireField(fields, "READY_FOR_REVIEW")
	if err != nil {
		return ImplementationResult{}, err
	}
	ready, err := strconv.ParseBool(readyStr)
	if err != nil {
		return ImplementationResult{}, fmt.Errorf("result field READY_FOR_REVIEW=%q is not a boolean", readyStr)
	}
	blockerCount := 0
	if v, ok := fields["BLOCKER_COUNT"]; ok {
		blockerCount, err = strconv.Atoi(v)
		if err != nil {
			return ImplementationResult{}, fmt.Errorf("result field BLOCKER_COUNT=%q is not an integer", v)
		}
	}
	if validation == "PASS" && !ready {
		return ImplementationResult{}, fmt.Errorf("inconsistent result: VALIDATION=PASS but READY_FOR_REVIEW=false")
	}
	return ImplementationResult{
		Validation:     validation,
		ReadyForReview: ready,
		BlockerCount:   blockerCount,
		Blockers:       fields["BLOCKERS"],
		Raw:            output,
	}, nil
}
