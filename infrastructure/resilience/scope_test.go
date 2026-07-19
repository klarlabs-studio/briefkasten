package resilience

import (
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// scopedStub is a Mailbox that knows read from unread mail.
type scopedStub struct{ *flakyMailbox }

func (s scopedStub) List(scope domain.Scope) ([]string, error) {
	switch scope {
	case domain.ScopeUnread:
		return []string{"1"}, nil
	case domain.ScopeRead:
		return []string{"2"}, nil
	case domain.ScopeAll:
		return []string{"1", "2"}, nil
	default:
		return nil, domain.ErrBadScope
	}
}

func (s scopedStub) SearchScope(scope domain.Scope, _ string) ([]string, error) {
	return s.List(scope)
}

// A scoped backend's List must survive the retry that the first call
// fails into — the pipeline wraps scoped listing like every other call.
func TestResilientListForwardsScope(t *testing.T) {
	inner := scopedStub{&flakyMailbox{failures: 1}}
	mb := Wrap(inner, Config{MaxAttempts: 3})

	ids, err := mb.List(domain.ScopeRead)
	if err != nil {
		t.Fatalf("List(read): %v", err)
	}
	if len(ids) != 1 || ids[0] != "2" {
		t.Fatalf("List(read) = %v, want [2]", ids)
	}

	hits, err := mb.SearchScope(domain.ScopeAll, "anything")
	if err != nil {
		t.Fatalf("SearchScope(all): %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("SearchScope(all) = %v, want 2 ids", hits)
	}
}

// A backend that only knows the unread backlog keeps working for unread
// and fails clearly for anything wider.
func TestResilientListUnscopedBackend(t *testing.T) {
	mb := Wrap(&flakyMailbox{}, Config{MaxAttempts: 1})

	if _, err := mb.List(domain.ScopeUnread); err != nil {
		t.Fatalf("List(unread): %v", err)
	}
	_, err := mb.List(domain.ScopeAll)
	if err == nil || !strings.Contains(err.Error(), "unread mail only") {
		t.Fatalf("List(all) error = %v, want an unsupported-scope error", err)
	}
}
