package maildir

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// Watcher reports new mail in a maildir by watching its new/ directory for
// file creation. It implements domain.MailboxWatcher.
type Watcher struct {
	newDir string
}

// NewWatcher watches root/new for arriving messages. The directory must exist
// (maildir.New creates it).
func NewWatcher(root string) *Watcher {
	return &Watcher{newDir: filepath.Join(root, "new")}
}

// Watch blocks until ctx is cancelled, calling onChange whenever a file lands
// in new/ (a message arriving). Create and Rename both count: maildir
// delivery may write to a tmp name and rename into new/.
func (w *Watcher) Watch(ctx context.Context, onChange func()) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("maildir watch: %w", err)
	}
	defer func() { _ = fsw.Close() }()

	if err := fsw.Add(w.newDir); err != nil {
		return fmt.Errorf("maildir watch %s: %w", w.newDir, err)
	}

	const arrival = fsnotify.Create | fsnotify.Rename
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if event.Op&arrival != 0 {
				onChange()
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("maildir watch: %w", err)
		}
	}
}
