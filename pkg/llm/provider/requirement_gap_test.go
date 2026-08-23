package provider

import (
	"net/http"
	"testing"
)

func TestRequirementGapVariantsUseSameUnsupportedStatus(t *testing.T) {
	if got := (&RequirementGap{}).StatusCode(); got != http.StatusNotImplemented {
		t.Fatalf("RequirementGap status = %d, want 501", got)
	}
	if got := (&RequirementGapsError{}).StatusCode(); got != http.StatusNotImplemented {
		t.Fatalf("RequirementGapsError status = %d, want 501", got)
	}
}
