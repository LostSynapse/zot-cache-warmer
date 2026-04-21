package main

import (
	"testing"

	"github.com/lostsynapse/zot-cache-warmer/internal/processor"
)

// TestExitCode covers the matrix of (stats, mode) → exit code. This is the
// contract CI pipelines depend on — strict mode must return non-zero for warm
// failures, soft mode must not.
func TestExitCode(t *testing.T) {
	tests := []struct {
		name  string
		stats processor.Stats
		soft  bool
		want  int
	}{
		{"clean-strict", processor.Stats{Total: 3, Parsed: 3, Cached: 3}, false, exitOK},
		{"clean-soft", processor.Stats{Total: 3, Parsed: 3, Cached: 3}, true, exitOK},
		{"all-warmed-strict", processor.Stats{Total: 3, Parsed: 3, Warmed: 3}, false, exitOK},
		{"mixed-strict", processor.Stats{Total: 3, Parsed: 3, Cached: 1, Warmed: 2}, false, exitOK},

		{"parse-error-strict", processor.Stats{Total: 3, Parsed: 2, ParseErrors: 1}, false, exitHard},
		{"parse-error-soft", processor.Stats{Total: 3, Parsed: 2, ParseErrors: 1}, true, exitHard},

		{"warm-error-strict", processor.Stats{Total: 3, Parsed: 3, Warmed: 2, WarmErrors: 1}, false, exitSoftWarm},
		{"warm-error-soft", processor.Stats{Total: 3, Parsed: 3, Warmed: 2, WarmErrors: 1}, true, exitOK},

		// Skipped-no-mapping is informational: never a failure on its own.
		{"skipped-strict", processor.Stats{Total: 3, Parsed: 2, SkippedNoMapping: 1, Cached: 2}, false, exitOK},
		{"skipped-soft", processor.Stats{Total: 3, Parsed: 2, SkippedNoMapping: 1, Cached: 2}, true, exitOK},

		// Probe errors alone don't escalate; the warmer falls through to warm
		// and the outcome is reflected in Warmed or WarmErrors.
		{"probe-only-strict", processor.Stats{Total: 3, Parsed: 3, ProbeErrors: 2, Warmed: 3}, false, exitOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := exitCode(tc.stats, tc.soft)
			if got != tc.want {
				t.Errorf("exitCode(%+v, soft=%v) = %d, want %d", tc.stats, tc.soft, got, tc.want)
			}
		})
	}
}

func TestStrictOrSoft(t *testing.T) {
	if strictOrSoft(true) != "soft" {
		t.Error("strictOrSoft(true) should be soft")
	}
	if strictOrSoft(false) != "strict" {
		t.Error("strictOrSoft(false) should be strict")
	}
}
