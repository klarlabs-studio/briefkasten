// Package application holds briefkasten's use cases — the single code
// path shared by every interface: the MCP tools, the CLI, and any future
// surface all call these methods. Confirmation of destructive operations
// is an interface concern (MCP elicitation, CLI prompt); the use cases
// here execute after approval.
package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.klarlabs.de/briefkasten/domain"
)

// Service exposes the mailbox use cases over a default mailbox and any
// named accounts.
type Service struct {
	mailbox  domain.Mailbox
	accounts map[string]domain.Mailbox
}

// NewService wires the use cases.
func NewService(mailbox domain.Mailbox, accounts map[string]domain.Mailbox) *Service {
	return &Service{mailbox: mailbox, accounts: accounts}
}

// Resolve routes an optional account and folder to the target mailbox.
func (s *Service) Resolve(ctx context.Context, account, folder string) (domain.Mailbox, error) {
	box := s.mailbox
	if account != "" {
		named, ok := s.accounts[account]
		if !ok {
			return nil, fmt.Errorf("briefkasten: unknown account %q", account)
		}
		box = named
	}
	if folder == "" {
		return box, nil
	}
	fm, ok := box.(domain.FolderMailbox)
	if !ok {
		return nil, errors.New("briefkasten: backend has no folder support")
	}
	return fm.InFolder(ctx, folder)
}

// ListUnread returns the unread ids of the resolved mailbox.
func (s *Service) ListUnread(ctx context.Context, account, folder string) ([]string, error) {
	return s.List(ctx, account, folder, domain.ScopeUnread)
}

// List returns the ids of the resolved mailbox within scope: the unread
// backlog, the already-read mail, or both. An empty scope means unread,
// so callers that predate scopes are unaffected.
func (s *Service) List(ctx context.Context, account, folder string, scope domain.Scope) ([]string, error) {
	scope, err := domain.ParseScope(string(scope))
	if err != nil {
		return nil, err
	}
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	ids, err := listMailbox(ctx, box, scope)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// Read returns the raw message bytes.
func (s *Service) Read(ctx context.Context, account, folder, id string) ([]byte, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	return box.Fetch(ctx, id)
}

// ReadMany returns the raw bytes for a batch of messages, reporting the
// outcome per id. The batch is measured before anything is read and
// refused whole if it would exceed domain.MaxFetchBytes.
func (s *Service) ReadMany(ctx context.Context, account, folder string, ids []string) (domain.FetchResult, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return domain.FetchResult{}, err
	}
	return fetchMany(ctx, box, ids)
}

// MarkSeen acknowledges a processed message. Idempotent: a message that
// is already read stays read and the call succeeds.
func (s *Service) MarkSeen(ctx context.Context, account, folder, id string) error {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return err
	}
	return box.MarkSeen(ctx, id)
}

// MarkSeenMany acknowledges a batch of processed messages, reporting the
// outcome per id. Idempotent per id, exactly like MarkSeen.
func (s *Service) MarkSeenMany(ctx context.Context, account, folder string, ids []string) (domain.BulkResult, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return domain.BulkResult{}, err
	}
	return markSeenMany(ctx, box, ids)
}

// Search finds unread messages matching the query. Backends with a
// Searcher search natively; everything else gets the scan fallback.
func (s *Service) Search(ctx context.Context, account, folder, query string) ([]string, error) {
	return s.SearchScope(ctx, account, folder, query, domain.ScopeUnread)
}

// SearchScope finds messages within scope matching the query.
func (s *Service) SearchScope(ctx context.Context, account, folder, query string, scope domain.Scope) ([]string, error) {
	scope, err := domain.ParseScope(string(scope))
	if err != nil {
		return nil, err
	}
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	ids, err := searchMailbox(ctx, box, scope, query)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// Folders lists the resolved account's folders.
func (s *Service) Folders(ctx context.Context, account string) ([]string, error) {
	box, err := s.Resolve(ctx, account, "")
	if err != nil {
		return nil, err
	}
	if fm, ok := box.(domain.FolderMailbox); ok {
		return fm.Folders(ctx)
	}
	return []string{"INBOX"}, nil
}

// Accounts returns the configured account names; "default" is first.
func (s *Service) Accounts() []string {
	names := []string{"default"}
	for name := range s.accounts {
		names = append(names, name)
	}
	sort.Strings(names[1:])
	return names
}

// Archive files a message away — soft, never destroyed. Read and unread
// messages curate alike; ids come from any scope of List or SearchScope.
// The caller must have obtained human confirmation.
func (s *Service) Archive(ctx context.Context, account, folder, id string) error {
	cu, err := s.curator(ctx, account, folder)
	if err != nil {
		return err
	}
	return cu.Archive(ctx, id)
}

// Delete moves a message to trash — soft delete, never expunged. Read
// and unread messages curate alike. The caller must have obtained human
// confirmation.
func (s *Service) Delete(ctx context.Context, account, folder, id string) error {
	cu, err := s.curator(ctx, account, folder)
	if err != nil {
		return err
	}
	return cu.Delete(ctx, id)
}

// ArchiveMany files a batch of messages away, reporting the outcome per
// id. The caller must have obtained human confirmation for the batch as
// a whole — one gesture, stated blast radius.
func (s *Service) ArchiveMany(ctx context.Context, account, folder string, ids []string) (domain.BulkResult, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return domain.BulkResult{}, err
	}
	return curateMany(ctx, box, ids, actionArchive)
}

// DeleteMany moves a batch of messages to trash, reporting the outcome
// per id. Soft, like Delete: nothing is expunged, and nothing is rolled
// back if part of the batch fails.
func (s *Service) DeleteMany(ctx context.Context, account, folder string, ids []string) (domain.BulkResult, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return domain.BulkResult{}, err
	}
	return curateMany(ctx, box, ids, actionDelete)
}

// CurationPlan reports where Archive and Delete would file, without
// moving anything — so a human can check the destination before
// approving a move rather than inferring it from where mail landed.
func (s *Service) CurationPlan(ctx context.Context, account, folder string) (domain.CurationPlan, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return domain.CurationPlan{}, err
	}
	ci, ok := box.(domain.CurationInspector)
	if !ok {
		return domain.CurationPlan{}, errors.New("briefkasten: backend cannot report curation destinations")
	}
	return ci.CurationPlan(ctx)
}

func (s *Service) curator(ctx context.Context, account, folder string) (domain.Curator, error) {
	box, err := s.Resolve(ctx, account, folder)
	if err != nil {
		return nil, err
	}
	cu, ok := box.(domain.Curator)
	if !ok {
		return nil, errors.New("briefkasten: backend has no curation support")
	}
	return cu, nil
}

// bulkEach is the shared bulk fallback: it applies op to one id at a
// time and collects the outcomes, so a backend without native batching
// gets correct per-id behaviour for free. The maildir backend wants
// exactly this — its operations are local renames with no round trip to
// save, so bespoke batching there would be extra code buying nothing.
//
// Every id gets its own outcome. A failure is recorded and the loop
// continues: the whole point of the per-id report is that one unusable
// id does not sink the rest of the batch.
//
// Cancellation is the one thing that stops the loop. It is returned as
// an error rather than recorded per id because nobody is waiting for the
// report any more, and the ids not yet attempted were not failures — the
// partial result travels with the error for a caller that wants it.
func bulkEach(ctx context.Context, ids []string, op func(context.Context, string) error) (domain.BulkResult, error) {
	res := domain.NewBulkResult(len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := op(ctx, id); err != nil {
			res.Fail(id, err)
			continue
		}
		res.Succeed(id)
	}
	return res, nil
}

// fetchEach is bulkEach for an operation whose successes carry bytes.
// Same contract: one unreadable id is that id's own failure, and only
// cancellation stops the loop.
func fetchEach(
	ctx context.Context, ids []string, fetch func(context.Context, string) ([]byte, error),
) (domain.FetchResult, error) {
	res := domain.NewFetchResult(len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		raw, err := fetch(ctx, id)
		if err != nil {
			res.Fail(id, err)
			continue
		}
		res.Add(id, raw)
	}
	return res, nil
}

// fetchMany reads a batch: natively when the backend batches, id by id
// otherwise. The id cap is checked here, before any dispatch, so it holds
// for every backend.
//
// A backend that batches natively pre-flights the size itself — it can
// measure the whole set in the same round trip it would spend anyway.
// Everything else is measured here, before the loop starts, because
// discovering the size while reading has already spent the memory.
func fetchMany(ctx context.Context, mb domain.Mailbox, ids []string) (domain.FetchResult, error) {
	if err := domain.CheckBulkIDs(ids); err != nil {
		return domain.FetchResult{}, err
	}
	if bf, ok := mb.(domain.BulkFetcher); ok {
		return bf.FetchMany(ctx, ids)
	}
	if err := preflightFetch(ctx, mb, ids); err != nil {
		return domain.FetchResult{}, err
	}
	return fetchEach(ctx, ids, mb.Fetch)
}

// preflightFetch measures a batch before a byte of it is read.
//
// A backend that cannot measure is refused rather than indulged: the
// alternative is an unbounded response, and "we could not tell how big
// this was going to be" is not a reason to find out the expensive way.
// The single-message fetch is untouched and remains the way through.
//
// Ids the backend does not hold are absent from the measurement and
// counted out of the total. They cost nothing to return, and they become
// their own failures when the fetch itself runs.
func preflightFetch(ctx context.Context, mb domain.Mailbox, ids []string) error {
	sizer, ok := mb.(domain.MessageSizer)
	if !ok {
		return errors.New(
			"briefkasten: backend cannot measure message sizes, so a batch fetch cannot be bounded — fetch ids one at a time")
	}
	sizes, err := sizer.Sizes(ctx, ids)
	if err != nil {
		return err
	}
	var (
		total    int64
		measured int
	)
	for _, id := range ids {
		size, held := sizes[id]
		if !held {
			continue
		}
		total += size
		measured++
	}
	return domain.CheckFetchBudget(total, measured)
}

// markSeenMany acknowledges a batch: natively when the backend batches,
// id by id otherwise.
func markSeenMany(ctx context.Context, mb domain.Mailbox, ids []string) (domain.BulkResult, error) {
	if err := domain.CheckBulkIDs(ids); err != nil {
		return domain.BulkResult{}, err
	}
	if bm, ok := mb.(domain.BulkMailbox); ok {
		return bm.MarkSeenMany(ctx, ids)
	}
	return bulkEach(ctx, ids, mb.MarkSeen)
}

// curateMany files a batch away. The cap is checked here, before any
// dispatch, so it holds for every backend rather than only the ones that
// batch natively.
func curateMany(
	ctx context.Context, mb domain.Mailbox, ids []string, action curationAction,
) (domain.BulkResult, error) {
	if err := domain.CheckBulkIDs(ids); err != nil {
		return domain.BulkResult{}, err
	}
	if bc, ok := mb.(domain.BulkCurator); ok {
		if action == actionArchive {
			return bc.ArchiveMany(ctx, ids)
		}
		return bc.DeleteMany(ctx, ids)
	}
	cu, ok := mb.(domain.Curator)
	if !ok {
		return domain.BulkResult{}, errors.New("briefkasten: backend has no curation support")
	}
	if action == actionArchive {
		return bulkEach(ctx, ids, cu.Archive)
	}
	return bulkEach(ctx, ids, cu.Delete)
}

// curationAction names which soft move a bulk call performs, so the two
// paths share one implementation instead of drifting apart.
type curationAction string

const (
	actionArchive curationAction = "archive"
	actionDelete  curationAction = "delete"
)

// listMailbox lists via the backend's ScopedMailbox when available. A
// backend without it can only speak for the unread backlog, so a wider
// scope fails loudly instead of quietly returning unread mail.
func listMailbox(ctx context.Context, mb domain.Mailbox, scope domain.Scope) ([]string, error) {
	if sm, ok := mb.(domain.ScopedMailbox); ok {
		return sm.List(ctx, scope)
	}
	if scope != domain.ScopeUnread {
		return nil, fmt.Errorf("briefkasten: backend lists unread mail only, scope %q unsupported", string(scope))
	}
	return mb.ListUnread(ctx)
}

// searchMailbox searches via the backend's ScopedSearcher or Searcher
// when available, otherwise falls back to scanning the scope's ids.
func searchMailbox(ctx context.Context, mb domain.Mailbox, scope domain.Scope, query string) ([]string, error) {
	if s, ok := mb.(domain.ScopedSearcher); ok {
		return s.SearchScope(ctx, scope, query)
	}
	if s, ok := mb.(domain.Searcher); ok && scope == domain.ScopeUnread {
		return s.Search(ctx, query)
	}
	ids, err := listMailbox(ctx, mb, scope)
	if err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		raw, err := mb.Fetch(ctx, id)
		if err != nil {
			// Skip what cannot be read, but stop once the caller's time
			// is up rather than walking the rest of the backlog to
			// collect the same error id by id.
			if ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		if bytes.Contains(bytes.ToLower(raw), needle) {
			out = append(out, id)
		}
	}
	return out, nil
}
