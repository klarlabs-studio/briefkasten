package resilience

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/domain"
)

// stubMailbox is a configurable Mailbox-only fake: it implements none of
// the optional capabilities, so it exercises the fallback paths.
type stubMailbox struct {
	ids       []string
	raws      map[string][]byte
	fetchErrs map[string]error
	seenErr   error
	seenCalls int
	fetches   int
}

func (s *stubMailbox) ListUnread(context.Context) ([]string, error) { return s.ids, nil }

func (s *stubMailbox) Fetch(_ context.Context, id string) ([]byte, error) {
	s.fetches++
	if err := s.fetchErrs[id]; err != nil {
		return nil, err
	}
	return s.raws[id], nil
}

func (s *stubMailbox) MarkSeen(context.Context, string) error {
	s.seenCalls++
	return s.seenErr
}

// searchingMailbox adds the domain.Searcher capability.
type searchingMailbox struct {
	stubMailbox
	query string
	hits  []string
}

func (s *searchingMailbox) Search(_ context.Context, query string) ([]string, error) {
	s.query = query
	return s.hits, nil
}

// folderedMailbox adds the domain.FolderMailbox capability.
type folderedMailbox struct {
	stubMailbox
	folders []string
}

func (f *folderedMailbox) Folders(context.Context) ([]string, error) { return f.folders, nil }

func (f *folderedMailbox) InFolder(_ context.Context, name string) (domain.Mailbox, error) {
	if slices.Contains(f.folders, name) {
		return &stubMailbox{ids: []string{name + "-1"}}, nil
	}
	return nil, fmt.Errorf("no such folder %q", name)
}

// curatedMailbox adds the domain.Curator capability.
type curatedMailbox struct {
	stubMailbox
	archived []string
	deleted  []string
}

func (c *curatedMailbox) Archive(_ context.Context, id string) error {
	c.archived = append(c.archived, id)
	return nil
}

func (c *curatedMailbox) Delete(_ context.Context, id string) error {
	c.deleted = append(c.deleted, id)
	return nil
}

func TestResilientMarkSeen(t *testing.T) {
	mb := &stubMailbox{}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	if err := r.MarkSeen(t.Context(), "m1.eml"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if mb.seenCalls != 1 {
		t.Errorf("calls = %d, want 1", mb.seenCalls)
	}
}

func TestResilientMarkSeenBadIDNotRetried(t *testing.T) {
	mb := &stubMailbox{seenErr: fmt.Errorf("%w: escape", domain.ErrBadID)}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	err := r.MarkSeen(t.Context(), "../../etc/passwd")
	if !errors.Is(err, domain.ErrBadID) {
		t.Fatalf("err = %v, want domain.ErrBadID", err)
	}
	// A bad id is the caller's mistake, not a backend fault: no retries.
	if mb.seenCalls != 1 {
		t.Errorf("calls = %d, want 1 (bad ids must not be retried)", mb.seenCalls)
	}
}

func TestResilientSearchDelegatesToSearcher(t *testing.T) {
	mb := &searchingMailbox{hits: []string{"hit.eml"}}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	ids, err := r.Search(t.Context(), "Rechnung")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ids) != 1 || ids[0] != "hit.eml" {
		t.Errorf("ids = %v, want [hit.eml]", ids)
	}
	if mb.query != "Rechnung" {
		t.Errorf("query = %q, want delegation to backend Searcher", mb.query)
	}
}

func TestResilientSearchFallbackScans(t *testing.T) {
	mb := &stubMailbox{
		ids: []string{"match.eml", "broken.eml"},
		raws: map[string][]byte{
			"match.eml": []byte("From: a@b\r\nSubject: RECHNUNG 42\r\n\r\nhi"),
		},
		fetchErrs: map[string]error{
			"broken.eml": errors.New("fetch exploded"),
		},
	}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	ids, err := r.Search(t.Context(), "rechnung")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Case-insensitive match; the unfetchable message is skipped silently.
	if len(ids) != 1 || ids[0] != "match.eml" {
		t.Errorf("ids = %v, want [match.eml]", ids)
	}
}

func TestResilientFoldersDelegates(t *testing.T) {
	mb := &folderedMailbox{folders: []string{"INBOX", "steuern"}}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	folders, err := r.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 || folders[0] != "INBOX" || folders[1] != "steuern" {
		t.Errorf("folders = %v", folders)
	}
}

func TestResilientFoldersWithoutSupport(t *testing.T) {
	r := Wrap(&stubMailbox{}, Config{InitialDelay: time.Millisecond})

	folders, err := r.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 1 || folders[0] != "INBOX" {
		t.Errorf("folders = %v, want [INBOX]", folders)
	}
}

func TestResilientInFolderDelegatesAndWraps(t *testing.T) {
	mb := &folderedMailbox{folders: []string{"INBOX", "steuern"}}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	scoped, err := r.InFolder(t.Context(), "steuern")
	if err != nil {
		t.Fatalf("InFolder: %v", err)
	}
	if scoped == nil {
		t.Fatal("scoped mailbox is nil")
	}
	if _, ok := scoped.(*Mailbox); !ok {
		t.Fatalf("scoped = %T, want resilience-wrapped *Mailbox", scoped)
	}
	ids, err := scoped.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("scoped ListUnread: %v", err)
	}
	if len(ids) != 1 || ids[0] != "steuern-1" {
		t.Errorf("scoped ids = %v", ids)
	}

	if _, err := r.InFolder(t.Context(), "nope"); err == nil {
		t.Error("unknown folder accepted")
	}
}

func TestResilientInFolderWithoutSupport(t *testing.T) {
	r := Wrap(&stubMailbox{ids: []string{"m1.eml"}}, Config{InitialDelay: time.Millisecond})

	scoped, err := r.InFolder(t.Context(), "INBOX")
	if err != nil {
		t.Fatalf("InFolder INBOX: %v", err)
	}
	var want domain.Mailbox = r
	if scoped != want {
		t.Errorf("scoped = %v, want the wrapper itself for INBOX", scoped)
	}

	if _, err := r.InFolder(t.Context(), "steuern"); err == nil || !strings.Contains(err.Error(), "folder") {
		t.Errorf("err = %v, want no-folder-support error", err)
	}
}

func TestResilientCurationDelegates(t *testing.T) {
	mb := &curatedMailbox{}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	if err := r.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if len(mb.archived) != 1 || mb.archived[0] != "a.eml" {
		t.Errorf("archived = %v", mb.archived)
	}
	if err := r.Delete(t.Context(), "d.eml"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(mb.deleted) != 1 || mb.deleted[0] != "d.eml" {
		t.Errorf("deleted = %v", mb.deleted)
	}
}

func TestResilientCurationWithoutSupport(t *testing.T) {
	r := Wrap(&stubMailbox{}, Config{InitialDelay: time.Millisecond})

	if err := r.Archive(t.Context(), "a.eml"); err == nil || !strings.Contains(err.Error(), "curation") {
		t.Errorf("Archive err = %v, want curation-capability error", err)
	}
	if err := r.Delete(t.Context(), "d.eml"); err == nil || !strings.Contains(err.Error(), "curation") {
		t.Errorf("Delete err = %v, want curation-capability error", err)
	}
}
