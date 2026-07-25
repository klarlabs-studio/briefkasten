package application_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/imap"
	"go.klarlabs.de/briefkasten/infrastructure/maildir"
)

// scanBox is a bare Mailbox — no Searcher, no ScopedSearcher, nothing
// optional — so a search over it can only take the scan fallback. It
// counts the reads the scan makes, which is the only way to see whether
// the fallback ran at all and how far it got.
type scanBox struct {
	ids     []string
	body    func(id string) string
	fetches int
}

// newScanBox builds a bare backend holding n messages, of which the one
// named in hit carries the needle.
func newScanBox(n int, hit string) *scanBox {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%d.eml", i)
	}
	return &scanBox{
		ids: ids,
		body: func(id string) string {
			if id == hit {
				return "Subject: Rechnung\r\n\r\nBetrag"
			}
			return "Subject: Newsletter\r\n\r\nnichts"
		},
	}
}

func (s *scanBox) ListUnread(context.Context) ([]string, error) { return s.ids, nil }

func (s *scanBox) Fetch(_ context.Context, id string) ([]byte, error) {
	s.fetches++
	return []byte(s.body(id)), nil
}

func (s *scanBox) MarkSeen(context.Context, string) error { return nil }

// A mailbox the fallback cannot read within its budget is refused whole.
// The alternative — scanning to the cap and answering with the matches
// found so far — would report an incomplete search as a complete one,
// which is the failure mode the budget exists to prevent.
func TestSearchFallbackRefusesOversizedMailbox(t *testing.T) {
	box := newScanBox(domain.MaxScanMessages+1, "m0.eml")
	svc := application.NewService(box, nil)

	ids, err := svc.Search(t.Context(), "", "", "rechnung")
	if !errors.Is(err, domain.ErrScanTooLarge) {
		t.Fatalf("Search over an oversized mailbox = %v (err %v), want ErrScanTooLarge", ids, err)
	}
	if len(ids) != 0 {
		t.Errorf("refused search returned %d ids, want none — a partial answer must not look like a whole one", len(ids))
	}
	// Named numbers: a caller that cannot see the cap cannot narrow the
	// search to fit it.
	for _, want := range []string{
		fmt.Sprint(domain.MaxScanMessages), fmt.Sprint(domain.MaxScanMessages + 1), "unread",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Refused before the first read, not part-way through it.
	if box.fetches != 0 {
		t.Errorf("backend saw %d reads, want none — the budget must be checked before the scan starts", box.fetches)
	}
}

// Exactly at the cap is inside the budget, and a mailbox under it is
// untouched by the bound: the fallback still reads every message and
// still answers with every match.
func TestSearchFallbackWithinBudgetIsUnaffected(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"small mailbox", 3},
		{"exactly at the cap", domain.MaxScanMessages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newScanBox(tc.n, "m1.eml")
			svc := application.NewService(box, nil)

			ids, err := svc.Search(t.Context(), "", "", "rechnung")
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(ids) != 1 || ids[0] != "m1.eml" {
				t.Fatalf("Search = %v, want [m1.eml]", ids)
			}
			if box.fetches != tc.n {
				t.Errorf("backend saw %d reads, want %d — the whole scope is scanned below the cap", box.fetches, tc.n)
			}
		})
	}
}

// searchSpy is a backend with a native search, recording that the search
// was the path taken and failing loudly if anything reads a message
// instead.
type searchSpy struct {
	*scanBox
	searches int
	scopes   []domain.Scope
}

func (s *searchSpy) Search(_ context.Context, _ string) ([]string, error) {
	s.searches++
	return []string{"native.eml"}, nil
}

// scopedSearchSpy is searchSpy with the scoped capability the two shipped
// backends actually implement.
type scopedSearchSpy struct{ *searchSpy }

func (s *scopedSearchSpy) SearchScope(_ context.Context, scope domain.Scope, _ string) ([]string, error) {
	s.searches++
	s.scopes = append(s.scopes, scope)
	return []string{"native.eml"}, nil
}

// A backend that searches natively never reaches the fallback, so the
// scan budget is not its business: a mailbox far over the cap still
// answers. Both shipped backends are in this position — see the
// compile-time assertions below.
func TestNativeSearchBypassesScanBudget(t *testing.T) {
	oversized := func() *searchSpy {
		return &searchSpy{scanBox: newScanBox(domain.MaxScanMessages*2, "m0.eml")}
	}

	t.Run("Searcher", func(t *testing.T) {
		spy := oversized()
		ids, err := application.NewService(spy, nil).Search(t.Context(), "", "", "rechnung")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(ids) != 1 || ids[0] != "native.eml" {
			t.Fatalf("Search = %v, want the native result", ids)
		}
		if spy.searches != 1 || spy.fetches != 0 {
			t.Errorf("backend saw %d searches and %d reads, want 1 search and no reads", spy.searches, spy.fetches)
		}
	})

	t.Run("ScopedSearcher", func(t *testing.T) {
		spy := &scopedSearchSpy{oversized()}
		svc := application.NewService(spy, nil)
		for _, scope := range []domain.Scope{domain.ScopeUnread, domain.ScopeRead, domain.ScopeAll} {
			ids, err := svc.SearchScope(t.Context(), "", "", "rechnung", scope)
			if err != nil {
				t.Fatalf("SearchScope(%s): %v", scope, err)
			}
			if len(ids) != 1 || ids[0] != "native.eml" {
				t.Fatalf("SearchScope(%s) = %v, want the native result", scope, ids)
			}
		}
		if spy.searches != 3 || spy.fetches != 0 {
			t.Errorf("backend saw %d searches and %d reads, want 3 searches and no reads", spy.searches, spy.fetches)
		}
		if len(spy.scopes) != 3 || spy.scopes[1] != domain.ScopeRead {
			t.Errorf("backend saw scopes %v, want each scope forwarded", spy.scopes)
		}
	})
}

// The two shipped backends implement ScopedSearcher, which is the first
// branch searchMailbox takes — so neither can reach the scan fallback and
// neither is affected by the budget. Asserting the capability is the
// whole proof: the fallback is unreachable for a type that satisfies it.
var (
	_ domain.ScopedSearcher = (*maildir.Mailbox)(nil)
	_ domain.ScopedSearcher = (*imap.Mailbox)(nil)
	_ domain.Searcher       = (*maildir.Mailbox)(nil)
	_ domain.Searcher       = (*imap.Mailbox)(nil)
)

// And the real maildir backend still searches as it did: a live search
// over a real mailbox, not just a type assertion.
func TestMaildirSearchUnchanged(t *testing.T) {
	mb, _ := newBulkDir(t, "a.eml", "b.eml")
	svc := application.NewService(mb, nil)

	ids, err := svc.Search(t.Context(), "", "", "a.eml")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Fatalf("Search = %v, want [a.eml]", ids)
	}
}
