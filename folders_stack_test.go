package briefkasten_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.klarlabs.de/briefkasten"
)

// The decorators are stacked in production — a runtime-swappable
// Switchable over a resilience-wrapped backend — so the capability has to
// survive both at once, not merely each on its own. This test fails if
// either forward is dropped.
func TestFolderManagementSurvivesBothDecorators(t *testing.T) {
	root := t.TempDir()
	dir, err := briefkasten.NewDirMailbox(root)
	if err != nil {
		t.Fatal(err)
	}
	stacked := briefkasten.NewSwitchable(briefkasten.Resilient(dir, briefkasten.ResilienceConfig{}))

	fm, ok := any(stacked).(briefkasten.FolderManager)
	if !ok {
		t.Fatal("the stacked decorators hide domain.FolderManager")
	}
	if err := fm.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Work", "new")); err != nil {
		t.Fatalf("folder not created through the stack: %v", err)
	}

	folders, err := stacked.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(folders, "Work") {
		t.Errorf("folders = %v, want Work", folders)
	}

	if err := fm.DeleteFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Work")); !os.IsNotExist(err) {
		t.Errorf("folder still on disk after delete through the stack: %v", err)
	}
}
