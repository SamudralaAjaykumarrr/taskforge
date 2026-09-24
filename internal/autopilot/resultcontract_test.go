package autopilot

import "testing"

func TestParseReviewResult_Approve(t *testing.T) {
	out := "some Claude prose here\n\nAUTOPILOT_RESULT_BEGIN\nVERDICT=APPROVE\nREADY=true\nBLOCKER_COUNT=0\nBLOCKERS=\nAUTOPILOT_RESULT_END\n"
	res, err := ParseReviewResult(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != "APPROVE" || !res.Ready || res.BlockerCount != 0 {
		t.Fatalf("unexpected parse: %+v", res)
	}
}

func TestParseReviewResult_BlockedRequiresBlockerType(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVERDICT=BLOCKED\nREADY=false\nBLOCKER_COUNT=1\nAUTOPILOT_RESULT_END\n"
	if _, err := ParseReviewResult(out); err == nil {
		t.Fatal("expected error: BLOCKED without BLOCKER_TYPE must fail closed")
	}
}

func TestParseReviewResult_ValidBlocked(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVERDICT=BLOCKED\nREADY=false\nBLOCKER_COUNT=1\nBLOCKER_TYPE=ordinary\nBLOCKERS=missing test for X\nAUTOPILOT_RESULT_END\n"
	res, err := ParseReviewResult(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BlockerType != BlockerOrdinary {
		t.Fatalf("expected BlockerOrdinary, got %s", res.BlockerType)
	}
}

func TestParseReviewResult_MissingBlock(t *testing.T) {
	if _, err := ParseReviewResult("no result block here at all"); err == nil {
		t.Fatal("expected error for missing result block, got nil (must fail closed)")
	}
}

func TestParseReviewResult_UnterminatedBlock(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVERDICT=APPROVE\nREADY=true\nBLOCKER_COUNT=0\n"
	if _, err := ParseReviewResult(out); err == nil {
		t.Fatal("expected error for unterminated result block")
	}
}

func TestParseReviewResult_MalformedLine(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVERDICT APPROVE\nAUTOPILOT_RESULT_END\n"
	if _, err := ParseReviewResult(out); err == nil {
		t.Fatal("expected error for malformed KEY=VALUE line")
	}
}

func TestParseReviewResult_InconsistentApproveWithBlockers(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVERDICT=APPROVE\nREADY=true\nBLOCKER_COUNT=2\nAUTOPILOT_RESULT_END\n"
	if _, err := ParseReviewResult(out); err == nil {
		t.Fatal("expected error: APPROVE with nonzero BLOCKER_COUNT is inconsistent")
	}
}

func TestParseReviewResult_InvalidVerdict(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVERDICT=MAYBE\nREADY=true\nBLOCKER_COUNT=0\nAUTOPILOT_RESULT_END\n"
	if _, err := ParseReviewResult(out); err == nil {
		t.Fatal("expected error for VERDICT value outside APPROVE/BLOCKED")
	}
}

func TestParseReviewResult_UsesLastBlockIfMultiplePresent(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVERDICT=BLOCKED\nREADY=false\nBLOCKER_COUNT=1\nBLOCKER_TYPE=ordinary\nAUTOPILOT_RESULT_END\n" +
		"\nmore text\n\nAUTOPILOT_RESULT_BEGIN\nVERDICT=APPROVE\nREADY=true\nBLOCKER_COUNT=0\nAUTOPILOT_RESULT_END\n"
	res, err := ParseReviewResult(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != "APPROVE" {
		t.Fatalf("expected the last block (APPROVE) to win, got %s", res.Verdict)
	}
}

func TestParseImplementationResult_Pass(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVALIDATION=PASS\nREADY_FOR_REVIEW=true\nBLOCKER_COUNT=0\nBLOCKERS=\nAUTOPILOT_RESULT_END\n"
	res, err := ParseImplementationResult(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Validation != "PASS" || !res.ReadyForReview {
		t.Fatalf("unexpected parse: %+v", res)
	}
}

func TestParseImplementationResult_InconsistentPassNotReady(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVALIDATION=PASS\nREADY_FOR_REVIEW=false\nAUTOPILOT_RESULT_END\n"
	if _, err := ParseImplementationResult(out); err == nil {
		t.Fatal("expected error: VALIDATION=PASS with READY_FOR_REVIEW=false is inconsistent")
	}
}

func TestParseImplementationResult_MissingValidation(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nREADY_FOR_REVIEW=true\nAUTOPILOT_RESULT_END\n"
	if _, err := ParseImplementationResult(out); err == nil {
		t.Fatal("expected error for missing VALIDATION field")
	}
}

func TestParseImplementationResult_Fail(t *testing.T) {
	out := "AUTOPILOT_RESULT_BEGIN\nVALIDATION=FAIL\nREADY_FOR_REVIEW=false\nBLOCKER_COUNT=1\nBLOCKERS=build broke\nAUTOPILOT_RESULT_END\n"
	res, err := ParseImplementationResult(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Validation != "FAIL" || res.BlockerCount != 1 {
		t.Fatalf("unexpected parse: %+v", res)
	}
}
