package application_test

import (
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// scopedBox is a memBox that also knows read from unread mail.
type scopedBox struct{ *memBox }

func (s scopedBox) List(scope domain.Scope) ([]string, error) {
	var ids []string
	for id := range s.msgs {
		if s.archived[id] || s.trashed[id] {
			continue
		}
		switch scope {
		case domain.ScopeUnread:
			if !s.seen[id] {
				ids = append(ids, id)
			}
		case domain.ScopeRead:
			if s.seen[id] {
				ids = append(ids, id)
			}
		case domain.ScopeAll:
			ids = append(ids, id)
		default:
			return nil, domain.ErrBadScope
		}
	}
	return ids, nil
}

func TestServiceListScopedBackend(t *testing.T) {
	box := scopedBox{newMemBox(map[string]string{"a": "alpha", "b": "beta"})}
	if err := box.MarkSeen("a"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	svc := application.NewService(box, nil)

	read, err := svc.List("", "", domain.ScopeRead)
	if err != nil {
		t.Fatalf("List(read): %v", err)
	}
	if len(read) != 1 || read[0] != "a" {
		t.Fatalf("List(read) = %v, want [a]", read)
	}

	all, err := svc.List("", "", domain.ScopeAll)
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List(all) = %v, want 2 ids", all)
	}
}

// An empty scope is the unread default, so pre-scope callers are safe.
func TestServiceListDefaultsToUnread(t *testing.T) {
	box := scopedBox{newMemBox(map[string]string{"a": "alpha", "b": "beta"})}
	if err := box.MarkSeen("a"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	svc := application.NewService(box, nil)

	ids, err := svc.List("", "", "")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("List(\"\") = %v, want [b]", ids)
	}
}

func TestServiceListRejectsUnknownScope(t *testing.T) {
	svc := application.NewService(newMemBox(map[string]string{"a": "alpha"}), nil)
	if _, err := svc.List("", "", domain.Scope("everything")); !errors.Is(err, domain.ErrBadScope) {
		t.Fatalf("List(everything) error = %v, want ErrBadScope", err)
	}
}

// A backend that only knows the unread backlog must say so rather than
// quietly handing back unread mail labelled as read.
func TestServiceListUnscopedBackendRejectsWiderScope(t *testing.T) {
	svc := application.NewService(newMemBox(map[string]string{"a": "alpha"}), nil)

	if _, err := svc.List("", "", domain.ScopeUnread); err != nil {
		t.Fatalf("List(unread) on plain backend: %v", err)
	}
	_, err := svc.List("", "", domain.ScopeRead)
	if err == nil || !strings.Contains(err.Error(), "unread mail only") {
		t.Fatalf("List(read) on plain backend error = %v, want an unsupported-scope error", err)
	}
}

func TestServiceSearchScope(t *testing.T) {
	box := scopedBox{newMemBox(map[string]string{"a": "alpha", "b": "beta"})}
	if err := box.MarkSeen("a"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	svc := application.NewService(box, nil)

	ids, err := svc.SearchScope("", "", "alpha", domain.ScopeRead)
	if err != nil {
		t.Fatalf("SearchScope: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("SearchScope(read, alpha) = %v, want [a]", ids)
	}

	// The unread default must not reach it.
	ids, err = svc.Search("", "", "alpha")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("Search(alpha) = %v, want none", ids)
	}
}

// The use cases route an id to the backend without consulting its read
// state — curating processed mail is the point of a wider scope.
func TestServiceCuratesReadMail(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   func(*application.Service, string) error
		gone func(scopedBox, string) bool
	}{
		{
			"archive",
			func(s *application.Service, id string) error { return s.Archive("", "", id) },
			func(b scopedBox, id string) bool { return b.archived[id] },
		},
		{
			"delete",
			func(s *application.Service, id string) error { return s.Delete("", "", id) },
			func(b scopedBox, id string) bool { return b.trashed[id] },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := scopedBox{newMemBox(map[string]string{"a": "alpha", "b": "beta"})}
			svc := application.NewService(box, nil)
			if err := svc.MarkSeen("", "", "a"); err != nil {
				t.Fatalf("MarkSeen: %v", err)
			}

			if err := tc.op(svc, "a"); err != nil {
				t.Fatalf("%s read message: %v", tc.name, err)
			}
			if !tc.gone(box, "a") {
				t.Errorf("%s did not move the read message", tc.name)
			}

			all, err := svc.List("", "", domain.ScopeAll)
			if err != nil {
				t.Fatalf("List(all): %v", err)
			}
			if len(all) != 1 || all[0] != "b" {
				t.Errorf("all after %s = %v, want [b]", tc.name, all)
			}
		})
	}
}

// Swapping the backend must swap the scoped view with it.
func TestSwitchableForwardsScope(t *testing.T) {
	a := scopedBox{newMemBox(map[string]string{"a": "alpha"})}
	if err := a.MarkSeen("a"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	sw := application.NewSwitchable(a)

	read, err := sw.List(domain.ScopeRead)
	if err != nil {
		t.Fatalf("List(read): %v", err)
	}
	if len(read) != 1 || read[0] != "a" {
		t.Fatalf("List(read) = %v, want [a]", read)
	}

	hits, err := sw.SearchScope(domain.ScopeRead, "alpha")
	if err != nil {
		t.Fatalf("SearchScope: %v", err)
	}
	if len(hits) != 1 || hits[0] != "a" {
		t.Fatalf("SearchScope(read, alpha) = %v, want [a]", hits)
	}

	// After the swap the new backend answers, unread this time.
	sw.Swap(scopedBox{newMemBox(map[string]string{"b": "beta"})})
	unread, err := sw.List(domain.ScopeUnread)
	if err != nil {
		t.Fatalf("List(unread) after swap: %v", err)
	}
	if len(unread) != 1 || unread[0] != "b" {
		t.Fatalf("List(unread) after swap = %v, want [b]", unread)
	}
}
