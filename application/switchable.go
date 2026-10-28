package application

import (
	"context"
	"errors"
	"sync"

	"go.klarlabs.de/briefkasten/domain"
)

// Switchable is a Mailbox whose backend can be swapped at runtime
// (runtime reconfiguration). All calls go to the current backend under a
// read lock; Swap replaces it atomically. Optional capabilities forward.
type Switchable struct {
	mu sync.RWMutex
	mb domain.Mailbox
}

// NewSwitchable wraps an initial backend.
func NewSwitchable(mb domain.Mailbox) *Switchable {
	return &Switchable{mb: mb}
}

// Swap replaces the backend for all subsequent calls.
func (s *Switchable) Swap(mb domain.Mailbox) {
	s.mu.Lock()
	s.mb = mb
	s.mu.Unlock()
}

func (s *Switchable) current() domain.Mailbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mb
}

// BulkLimit returns the active backend's cap or the default.
func (s *Switchable) BulkLimit() int {
	if p, ok := s.current().(domain.BulkLimitProvider); ok && p.BulkLimit() > 0 {
		return p.BulkLimit()
	}
	return domain.MaxBulkIDs
}

// SetBulkLimit applies a cap to the active backend when it supports tuning.
func (s *Switchable) SetBulkLimit(limit int) {
	if setter, ok := s.current().(domain.BulkLimitSetter); ok {
		setter.SetBulkLimit(limit)
	}
}

// ListUnread lists the current backend's unread ids.
func (s *Switchable) ListUnread(ctx context.Context) ([]string, error) {
	return s.current().ListUnread(ctx)
}

// List forwards to the backend's ScopedMailbox when it has one.
func (s *Switchable) List(ctx context.Context, scope domain.Scope) ([]string, error) {
	return listMailbox(ctx, s.current(), scope)
}

// Fetch returns the raw message bytes from the current backend.
func (s *Switchable) Fetch(ctx context.Context, id string) ([]byte, error) {
	return s.current().Fetch(ctx, id)
}

// FetchHeaders forwards the header-only capability to the active backend.
func (s *Switchable) FetchHeaders(ctx context.Context, id string) (domain.MessageHeaders, error) {
	fetcher, ok := s.current().(domain.HeaderFetcher)
	if !ok {
		return domain.MessageHeaders{}, errors.New("briefkasten: backend cannot fetch message headers")
	}
	return fetcher.FetchHeaders(ctx, id)
}

// FetchHeadersMany forwards or safely falls back to individual header reads.
func (s *Switchable) FetchHeadersMany(ctx context.Context, ids []string) (map[string]domain.MessageHeaders, map[string]error) {
	if fetcher, ok := s.current().(domain.BatchHeaderFetcher); ok {
		return fetcher.FetchHeadersMany(ctx, ids)
	}
	result := make(map[string]domain.MessageHeaders, len(ids))
	failures := make(map[string]error)
	fetcher, ok := s.current().(domain.HeaderFetcher)
	if !ok {
		for _, id := range ids {
			failures[id] = errors.New("briefkasten: backend cannot fetch message headers")
		}
		return result, failures
	}
	for _, id := range ids {
		h, err := fetcher.FetchHeaders(ctx, id)
		if err != nil {
			failures[id] = err
		} else {
			result[id] = h
		}
	}
	return result, failures
}

// FetchMany forwards to the backend's BulkFetcher, or pre-flights and
// loops. Without the forward the capability would vanish behind this
// decorator and every batch would cost one round trip per message.
func (s *Switchable) FetchMany(ctx context.Context, ids []string) (domain.FetchResult, error) {
	return fetchMany(ctx, s.current(), ids, s.BulkLimit())
}

// Sizes forwards to the backend's MessageSizer. A backend that cannot
// measure says so — the pre-flight must not be able to pass by default.
func (s *Switchable) Sizes(ctx context.Context, ids []string) (map[string]int64, error) {
	sizer, ok := s.current().(domain.MessageSizer)
	if !ok {
		return nil, errors.New("briefkasten: backend cannot measure message sizes")
	}
	return sizer.Sizes(ctx, ids)
}

// MarkSeen acknowledges a message on the current backend.
func (s *Switchable) MarkSeen(ctx context.Context, id string) error {
	return s.current().MarkSeen(ctx, id)
}

// MarkSeenMany forwards to the backend's BulkMailbox, or loops.
func (s *Switchable) MarkSeenMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return markSeenMany(ctx, s.current(), ids, s.BulkLimit())
}

// Search forwards to the backend's Searcher or the generic fallback.
func (s *Switchable) Search(ctx context.Context, query string) ([]string, error) {
	return searchMailbox(ctx, s.current(), domain.ScopeUnread, query)
}

// SearchScope forwards to the backend's ScopedSearcher or the fallback.
func (s *Switchable) SearchScope(ctx context.Context, scope domain.Scope, query string) ([]string, error) {
	return searchMailbox(ctx, s.current(), scope, query)
}

// SearchFromScope forwards sender-filtered searches to the active backend.
func (s *Switchable) SearchFromScope(ctx context.Context, scope domain.Scope, query, from string) ([]string, error) {
	if searcher, ok := s.current().(domain.SenderSearcher); ok {
		return searcher.SearchFromScope(ctx, scope, query, from)
	}
	return searchFromFallback(ctx, s.current(), scope, query, from)
}

// Folders forwards to the backend when it supports folders.
func (s *Switchable) Folders(ctx context.Context) ([]string, error) {
	if fm, ok := s.current().(domain.FolderMailbox); ok {
		return fm.Folders(ctx)
	}
	return []string{"INBOX"}, nil
}

// InFolder forwards to the backend when it supports folders.
func (s *Switchable) InFolder(ctx context.Context, name string) (domain.Mailbox, error) {
	if fm, ok := s.current().(domain.FolderMailbox); ok {
		return fm.InFolder(ctx, name)
	}
	if name == "INBOX" {
		return s, nil
	}
	return nil, errors.New("briefkasten: backend has no folder support")
}

// CreateFolder forwards to the backend's FolderManager. Without the
// forward the capability would vanish behind this decorator and every
// folder tool would report a backend that cannot manage folders.
func (s *Switchable) CreateFolder(ctx context.Context, name string) error {
	fm, ok := s.current().(domain.FolderManager)
	if !ok {
		return errNoFolderManagement
	}
	return fm.CreateFolder(ctx, name)
}

// DeleteFolder forwards to the backend's FolderManager. The refusals —
// non-empty, protected, bad name — are the backend's to make: they
// depend on what that backend holds and where its curation files, and a
// swap changes both.
func (s *Switchable) DeleteFolder(ctx context.Context, name string) error {
	fm, ok := s.current().(domain.FolderManager)
	if !ok {
		return errNoFolderManagement
	}
	return fm.DeleteFolder(ctx, name)
}

// Archive forwards to the backend's Curator.
func (s *Switchable) Archive(ctx context.Context, id string) error {
	cu, ok := s.current().(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	return cu.Archive(ctx, id)
}

// Delete forwards to the backend's Curator.
func (s *Switchable) Delete(ctx context.Context, id string) error {
	cu, ok := s.current().(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	return cu.Delete(ctx, id)
}

// Move forwards to the backend's Mover.
func (s *Switchable) Move(ctx context.Context, id, destination string) error {
	mover, ok := s.current().(domain.Mover)
	if !ok {
		return errors.New("briefkasten: backend has no move support")
	}
	return mover.Move(ctx, id, destination)
}

// ArchiveMany forwards to the backend's BulkCurator, or loops over its
// Curator. Without the forward the capability would vanish behind this
// decorator and every batch would silently fall back to one call per id.
func (s *Switchable) ArchiveMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return curateMany(ctx, s.current(), ids, actionArchive, s.BulkLimit())
}

// DeleteMany forwards to the backend's BulkCurator, or loops over its
// Curator.
func (s *Switchable) DeleteMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return curateMany(ctx, s.current(), ids, actionDelete, s.BulkLimit())
}

// MoveMany forwards a batch move or falls back to individual moves.
func (s *Switchable) MoveMany(ctx context.Context, ids []string, destination string) (domain.BulkResult, error) {
	return moveMany(ctx, s.current(), ids, destination, s.BulkLimit())
}

// CurationPlan forwards to the backend's inspector. The destinations
// belong to whichever backend is current, so a runtime swap changes the
// answer — which is exactly why it is asked rather than remembered.
func (s *Switchable) CurationPlan(ctx context.Context) (domain.CurationPlan, error) {
	ci, ok := s.current().(domain.CurationInspector)
	if !ok {
		return domain.CurationPlan{}, errors.New("briefkasten: backend cannot report curation destinations")
	}
	return ci.CurationPlan(ctx)
}

// errNoFolderManagement is the one answer both decorators and the
// service give for a backend that only lists folders. Shared so the
// three layers cannot drift into three different phrasings of the same
// missing capability.
var errNoFolderManagement = errors.New("briefkasten: backend cannot create or delete folders")

var (
	_ domain.Mailbox            = (*Switchable)(nil)
	_ domain.FolderManager      = (*Switchable)(nil)
	_ domain.ScopedMailbox      = (*Switchable)(nil)
	_ domain.Searcher           = (*Switchable)(nil)
	_ domain.ScopedSearcher     = (*Switchable)(nil)
	_ domain.HeaderFetcher      = (*Switchable)(nil)
	_ domain.BatchHeaderFetcher = (*Switchable)(nil)
	_ domain.BulkLimitProvider  = (*Switchable)(nil)
	_ domain.BulkLimitSetter    = (*Switchable)(nil)
	_ domain.FolderMailbox      = (*Switchable)(nil)
	_ domain.Curator            = (*Switchable)(nil)
	_ domain.Mover              = (*Switchable)(nil)
	_ domain.CurationInspector  = (*Switchable)(nil)
	_ domain.BulkMailbox        = (*Switchable)(nil)
	_ domain.BulkCurator        = (*Switchable)(nil)
	_ domain.BulkMover          = (*Switchable)(nil)
	_ domain.BulkFetcher        = (*Switchable)(nil)
	_ domain.MessageSizer       = (*Switchable)(nil)
)
