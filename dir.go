package briefkasten

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DirMailbox is the local-first backend: a maildir-style directory where
// new/ holds unread .eml files and cur/ holds seen ones. Dropping a file
// into new/ is "receiving mail" — ideal for development, testing, and
// pipelines that already export messages to disk.
type DirMailbox struct {
	root string
}

// NewDirMailbox prepares the directory layout (root/new, root/cur).
func NewDirMailbox(root string) (*DirMailbox, error) {
	for _, sub := range []string{"new", "cur"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("briefkasten: prepare %s: %w", sub, err)
		}
	}
	return &DirMailbox{root: root}, nil
}

// ListUnread returns message ids (filenames) in new/, in stable order.
func (m *DirMailbox) ListUnread() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, "new"))
	if err != nil {
		return nil, fmt.Errorf("briefkasten: list: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Fetch returns the raw message bytes for an unread id.
func (m *DirMailbox) Fetch(id string) ([]byte, error) {
	path, err := m.safePath("new", id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("briefkasten: fetch %q: %w", id, err)
	}
	return data, nil
}

// MarkSeen moves a message from new/ to cur/.
func (m *DirMailbox) MarkSeen(id string) error {
	from, err := m.safePath("new", id)
	if err != nil {
		return err
	}
	to, err := m.safePath("cur", id)
	if err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("briefkasten: mark seen %q: %w", id, err)
	}
	return nil
}

// safePath joins root/sub/id, rejecting ids that escape the mailbox.
func (m *DirMailbox) safePath(sub, id string) (string, error) {
	if id == "" || id != filepath.Base(id) || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("%w: %q", ErrBadID, id)
	}
	return filepath.Join(m.root, sub, id), nil
}

// Search scans the unread backlog for a case-insensitive substring match.
func (d *DirMailbox) Search(query string) ([]string, error) {
	ids, err := d.ListUnread()
	if err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		raw, err := d.Fetch(id)
		if err != nil {
			continue
		}
		if bytes.Contains(bytes.ToLower(raw), needle) {
			out = append(out, id)
		}
	}
	return out, nil
}

var _ Searcher = (*DirMailbox)(nil)
