// Package maildir is briefkasten's local-first backend: maildir-style
// directories on disk.
package maildir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/briefkasten/domain"
)

// Mailbox is the local-first backend: a maildir-style directory where
// new/ holds unread .eml files and cur/ holds seen ones. Dropping a file
// into new/ is "receiving mail" — ideal for development, testing, and
// pipelines that already export messages to disk.
//
// The context contract is honoured by checking it once at the entry to
// each operation. Everything below is local file I/O, which the kernel
// completes or fails in microseconds and which no context can interrupt
// anyway; running it on another goroutine to be able to walk away from
// it would buy cancellation of something that was never going to hang,
// at the cost of a rename racing a returned error. The check at the door
// is the whole of it: a caller whose deadline has already passed is not
// made to wait for work nobody is waiting for.
type Mailbox struct {
	root string
}

// New prepares the directory layout (root/new, root/cur).
func New(root string) (*Mailbox, error) {
	for _, sub := range []string{"new", "cur"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			return nil, fmt.Errorf("briefkasten: prepare %s: %w", sub, err)
		}
	}
	return &Mailbox{root: root}, nil
}

// ListUnread returns message ids (filenames) in new/, in stable order.
func (m *Mailbox) ListUnread(ctx context.Context) ([]string, error) {
	return m.List(ctx, domain.ScopeUnread)
}

// List returns message ids for the scope, in stable order: new/ for
// unread, cur/ for read, and the union of both for all.
func (m *Mailbox) List(ctx context.Context, scope domain.Scope) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("briefkasten: list: %w", err)
	}
	var subs []string
	switch scope {
	case domain.ScopeUnread:
		subs = []string{"new"}
	case domain.ScopeRead:
		subs = []string{"cur"}
	case domain.ScopeAll:
		subs = []string{"new", "cur"}
	default:
		return nil, fmt.Errorf("%w: %q", domain.ErrBadScope, string(scope))
	}

	seen := make(map[string]struct{})
	ids := []string{}
	for _, sub := range subs {
		entries, err := os.ReadDir(filepath.Join(m.root, sub))
		if err != nil {
			return nil, fmt.Errorf("briefkasten: list: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			// A message that was marked seen mid-listing can appear in
			// both dirs; report it once.
			if _, dup := seen[e.Name()]; dup {
				continue
			}
			seen[e.Name()] = struct{}{}
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

var _ domain.ScopedMailbox = (*Mailbox)(nil)

// Fetch returns the raw message bytes for an id, unread (new/) or
// already read (cur/). Reading never moves the message, so fetching a
// seen message leaves it seen and an unread one unread.
func (m *Mailbox) Fetch(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("briefkasten: fetch %q: %w", id, err)
	}
	for _, sub := range []string{"new", "cur"} {
		path, err := m.safePath(sub, id)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path) // #nosec G304 -- path built by safePath, which rejects ids that escape the mailbox
		if err == nil {
			return data, nil
		}
		// A missing file only rules out this directory. Anything else —
		// an unreadable mailbox, a failing disk — is the backend in
		// trouble and must surface as itself; swallowing it here would
		// report a real fault as a caller mistake and hide it from the
		// retry and circuit-breaker path.
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("briefkasten: fetch %q: %w", id, err)
		}
	}
	// In neither new/ nor cur/: the caller named a message that is not
	// here, which resilience must never retry nor hold against the
	// backend's health.
	return nil, fmt.Errorf("%w: %q", domain.ErrBadID, id)
}

// Sizes reports each id's size on disk without opening a single message.
// It is what bounds a batched fetch here: a stat per id is cheap enough
// that measuring first and reading second costs nothing worth saving,
// and it is the only way to refuse an oversized batch before the bytes
// are in memory.
//
// An id this mailbox does not hold is left out of the map rather than
// failing the call — it becomes its own failure when the fetch runs, and
// the ids that are here still get measured.
func (m *Mailbox) Sizes(ctx context.Context, ids []string) (map[string]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("briefkasten: sizes: %w", err)
	}
	out := make(map[string]int64, len(ids))
	for _, id := range ids {
		path, err := m.locate(id)
		if err != nil {
			// Unknown or unusable ids are simply not measured. Both are
			// answered per id by the fetch itself.
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			// The file was there a moment ago; a failing stat is the disk
			// in trouble, not a caller mistake, and must not be rounded
			// down to "zero bytes" in a budget check.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("briefkasten: size %q: %w", id, err)
		}
		out[id] = info.Size()
	}
	return out, nil
}

var _ domain.MessageSizer = (*Mailbox)(nil)

// MarkSeen moves a message from new/ to cur/. Acknowledging a message
// that is already read succeeds without doing anything — the tool is
// idempotent, and an agent re-acknowledging its work is not an error.
// An id in neither directory is a bad id, not a backend fault.
func (m *Mailbox) MarkSeen(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("briefkasten: mark seen %q: %w", id, err)
	}
	from, err := m.safePath("new", id)
	if err != nil {
		return err
	}
	to, err := m.safePath("cur", id)
	if err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("briefkasten: mark seen %q: %w", id, err)
		}
		if _, statErr := os.Stat(to); statErr == nil {
			return nil
		}
		return fmt.Errorf("%w: %q", domain.ErrBadID, id)
	}
	return nil
}

// locate resolves an id to its on-disk path, preferring the unread
// backlog (new/) and falling back to read mail (cur/).
func (m *Mailbox) locate(id string) (string, error) {
	newPath, err := m.safePath("new", id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(newPath); err == nil {
		return newPath, nil
	}
	curPath, err := m.safePath("cur", id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(curPath); err == nil {
		return curPath, nil
	}
	return "", fmt.Errorf("%w: %q", domain.ErrBadID, id)
}

// safePath joins root/sub/id, rejecting ids that escape the mailbox.
func (m *Mailbox) safePath(sub, id string) (string, error) {
	if id == "" || id != filepath.Base(id) || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("%w: %q", domain.ErrBadID, id)
	}
	return filepath.Join(m.root, sub, id), nil
}

// Search scans the unread backlog for a case-insensitive substring match.
func (d *Mailbox) Search(ctx context.Context, query string) ([]string, error) {
	return d.SearchScope(ctx, domain.ScopeUnread, query)
}

// SearchScope scans the scope's messages for a case-insensitive
// substring match.
func (d *Mailbox) SearchScope(ctx context.Context, scope domain.Scope, query string) ([]string, error) {
	ids, err := d.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		// A scan over a large backlog is the one place this backend can
		// run long, so the deadline is re-checked per message rather than
		// only at the door.
		raw, err := d.Fetch(ctx, id)
		if err != nil {
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

var (
	_ domain.Searcher       = (*Mailbox)(nil)
	_ domain.ScopedSearcher = (*Mailbox)(nil)
)

const (
	archiveFolder = "Archive"
	trashFolder   = "Trash"
	archiveDir    = ".archive"
	trashDir      = ".trash"
)

// curatedFolders maps the folder name briefkasten exposes to the maildir
// curation actually files into. The exposed names are the conventional
// IMAP leaf names the IMAP backend already uses ("Archive", "Trash"), so
// the same mail answers to the same folder name whichever backend an
// account runs on and a script written against one keeps working against
// the other. Those two names are therefore reserved on this backend. On
// disk the directories stay dot-prefixed: hidden from `ls`, and reachable
// only through this table — never through the generic scan in Folders or
// the free-form path in InFolder.
var curatedFolders = map[string]string{
	archiveFolder: archiveDir,
	trashFolder:   trashDir,
}

// curatedDir resolves an exposed folder name to its on-disk maildir. The
// on-disk names are accepted as well because CurationPlan reports those,
// and a caller that has just been told where mail would land must be able
// to act on that answer.
func curatedDir(name string) (string, bool) {
	if dir, ok := curatedFolders[name]; ok {
		return dir, true
	}
	for _, dir := range curatedFolders {
		if name == dir {
			return dir, true
		}
	}
	return "", false
}

// curatedName is curatedDir's inverse: the exposed name for an on-disk
// maildir, if that directory is one briefkasten curates into.
func curatedName(dir string) (string, bool) {
	for name, d := range curatedFolders {
		if d == dir {
			return name, true
		}
	}
	return "", false
}

// Folders lists the root maildir ("INBOX"), the curated maildirs under
// their exposed names, plus every subdirectory that looks like a maildir
// (contains new/).
func (d *Mailbox) Folders(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("briefkasten: folders: %w", err)
	}
	folders := []string{"INBOX"}
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, fmt.Errorf("briefkasten: folders: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "new" || e.Name() == "cur" {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			// Only the maildirs briefkasten curates into are mail; other
			// dot-directories (sync state, editor droppings) are not, and
			// stay invisible.
			curated, ok := curatedName(name)
			if !ok {
				continue
			}
			name = curated
		} else if _, reserved := curatedFolders[name]; reserved {
			// A plain directory under a reserved name would list twice
			// and shadow the curated one; the reserved name always means
			// the curated maildir.
			continue
		}
		if st, err := os.Stat(filepath.Join(d.root, e.Name(), "new")); err == nil && st.IsDir() {
			folders = append(folders, name)
		}
	}
	sort.Strings(folders[1:])
	return folders, nil
}

// InFolder returns a Mailbox over the named sub-maildir; "INBOX" is the
// root and the curated names reach archived and trashed mail. Folder
// names cannot escape the root.
func (d *Mailbox) InFolder(ctx context.Context, name string) (domain.Mailbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("briefkasten: folder %q: %w", name, err)
	}
	if name == "INBOX" {
		return d, nil
	}
	// Curated names resolve through the table, so the path component is
	// a constant from this file rather than anything the caller supplied.
	if dir, ok := curatedDir(name); ok {
		return New(filepath.Join(d.root, dir))
	}
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("briefkasten: invalid folder %q", name)
	}
	return New(filepath.Join(d.root, name))
}

var _ domain.FolderMailbox = (*Mailbox)(nil)

// moveTo relocates a message into a hidden sub-maildir. Read messages
// (cur/) curate just like unread ones (new/).
func (d *Mailbox) moveTo(ctx context.Context, sub, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("briefkasten: move %q to %s: %w", id, sub, err)
	}
	from, err := d.locate(id)
	if err != nil {
		return err
	}
	destDir := filepath.Join(d.root, sub, "new")
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("briefkasten: prepare %s: %w", sub, err)
	}
	if err := os.Rename(from, filepath.Join(destDir, id)); err != nil {
		return fmt.Errorf("briefkasten: move %q to %s: %w", id, sub, err)
	}
	return nil
}

// Archive moves a message — read or unread — to .archive/new: out of
// the backlog, never destroyed, and still listable and fetchable through
// the "Archive" folder.
func (d *Mailbox) Archive(ctx context.Context, id string) error {
	return d.moveTo(ctx, archiveDir, id)
}

// Delete moves a message — read or unread — to .trash/new: a soft
// delete; real removal stays a human decision outside briefkasten. The
// message stays reachable through the "Trash" folder until then.
func (d *Mailbox) Delete(ctx context.Context, id string) error {
	return d.moveTo(ctx, trashDir, id)
}

// CurationPlan reports where curation files messages. The dir backend
// owns its whole layout, so there is nothing to discover — the answer is
// the same every time, and reported only so the surface matches the IMAP
// backend rather than leaving humans to guess which one they are on.
func (d *Mailbox) CurationPlan(ctx context.Context) (domain.CurationPlan, error) {
	if err := ctx.Err(); err != nil {
		return domain.CurationPlan{}, fmt.Errorf("briefkasten: curation plan: %w", err)
	}
	return domain.CurationPlan{
		Archive: domain.CurationDestination{Folder: archiveDir, Route: domain.RouteFixed},
		Trash:   domain.CurationDestination{Folder: trashDir, Route: domain.RouteFixed},
	}, nil
}

var (
	_ domain.Curator           = (*Mailbox)(nil)
	_ domain.CurationInspector = (*Mailbox)(nil)
)
