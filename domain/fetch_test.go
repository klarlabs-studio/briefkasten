package domain_test

import (
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// The budget is a ceiling, not a target: a batch that measures exactly
// MaxFetchBytes is inside it, and the first byte past it is out. Which
// side the boundary falls on is the kind of thing that drifts silently
// under a refactor, so it is pinned here rather than inferred from a
// test that happens to sit well clear of it.
func TestCheckFetchBudgetBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int64
		want  bool // want a refusal
	}{
		{"empty batch", 0, false},
		{"one byte under", domain.MaxFetchBytes - 1, false},
		{"exactly the budget", domain.MaxFetchBytes, false},
		{"one byte over", domain.MaxFetchBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CheckFetchBudget(tc.total, 3)
			if tc.want != (err != nil) {
				t.Fatalf("CheckFetchBudget(%d) = %v, refusal wanted: %v", tc.total, err, tc.want)
			}
			if !tc.want {
				return
			}
			if !errors.Is(err, domain.ErrFetchTooLarge) {
				t.Errorf("error = %v, want ErrFetchTooLarge", err)
			}
			// A refusal a caller cannot act on is barely better than a
			// hang: it has to name the budget, what the batch measures,
			// and how many ids that covers.
			for _, want := range []string{"25 MiB", "3 ids", "26214401"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to name %q", err, want)
				}
			}
		})
	}
}
