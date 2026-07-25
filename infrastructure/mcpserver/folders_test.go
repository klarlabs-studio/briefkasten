package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// listedFolders reads the folder list the way a client does, so a create
// or a delete is checked against the surface rather than against the
// tool's own answer.
func listedFolders(t *testing.T, client interface{ ReadResource(string) (string, error) }) []string {
	t.Helper()
	text, err := client.ReadResource("email://folders")
	if err != nil {
		t.Fatalf("read email://folders: %v", err)
	}
	var payload struct {
		Folders []string `json:"folders"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("folders payload not JSON: %v (%q)", err, text)
	}
	return payload.Folders
}

// The resource is the folder list, so it has to move with the folders —
// a create and a delete that email://folders does not reflect would
// leave every client working from a stale picture.
func TestFolderToolsAreReflectedInTheResource(t *testing.T) {
	client, _ := newClient(t)

	if before := listedFolders(t, client); slices.Contains(before, "Work") {
		t.Fatalf("folders = %v, want no Work yet", before)
	}

	out := callMap(t, client, "email.folder_create", map[string]any{"name": "Work", "confirm": true})
	if out["ok"] != true {
		t.Fatalf("folder_create = %v, want ok", out)
	}
	if after := listedFolders(t, client); !slices.Contains(after, "Work") {
		t.Errorf("folders = %v, want Work listed after creating it", after)
	}

	// Idempotent: asking again is the state the caller wanted.
	if out := callMap(t, client, "email.folder_create", map[string]any{"name": "Work", "confirm": true}); out["ok"] != true {
		t.Fatalf("second folder_create = %v, want ok", out)
	}

	if out := callMap(t, client, "email.folder_delete", map[string]any{"name": "Work", "confirm": true}); out["ok"] != true {
		t.Fatalf("folder_delete = %v, want ok", out)
	}
	if after := listedFolders(t, client); slices.Contains(after, "Work") {
		t.Errorf("folders = %v, want Work gone after deleting it", after)
	}
}

// Both operations are gated. Message content reaches every tool, so a
// request to reshape the mailbox is as capable of originating in an
// email body as a request to delete mail.
func TestFolderToolsRequireConfirmation(t *testing.T) {
	client, _ := newClient(t)

	for _, tool := range []string{"email.folder_create", "email.folder_delete"} {
		_, err := client.CallToolRaw(tool, map[string]any{"name": "Work"})
		if err == nil || !strings.Contains(err.Error(), "confirmation required") {
			t.Errorf("%s without confirm: %v, want the confirmation gate", tool, err)
		}
	}
	// Nothing happened on the way to those refusals.
	if after := listedFolders(t, client); slices.Contains(after, "Work") {
		t.Errorf("folders = %v, want the ungated create to have done nothing", after)
	}
}

// The refusal a model most needs to understand before it proposes a
// delete: mail is never destroyed, and the count says how much is in the
// way.
func TestFolderDeleteRefusesAFolderHoldingMail(t *testing.T) {
	client, root := newClient(t)
	callMap(t, client, "email.folder_create", map[string]any{"name": "Work", "confirm": true})
	if err := os.WriteFile(filepath.Join(root, "Work", "new", "a.eml"), []byte("From: a\r\n\r\nx"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := client.CallToolRaw("email.folder_delete", map[string]any{"name": "Work", "confirm": true})
	if err == nil {
		t.Fatal("folder_delete of a folder holding mail succeeded")
	}
	for _, want := range []string{"not empty", "1 message", "archive or delete them first"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err, want)
		}
	}
	if after := listedFolders(t, client); !slices.Contains(after, "Work") {
		t.Errorf("folders = %v, want Work still there", after)
	}
	if _, statErr := os.Stat(filepath.Join(root, "Work", "new", "a.eml")); statErr != nil {
		t.Errorf("message gone after a refused delete: %v", statErr)
	}
}

func TestFolderDeleteRefusesInboxAndCurationDestinations(t *testing.T) {
	client, _ := newClient(t)

	for _, name := range []string{"INBOX", "Archive", "Trash"} {
		_, err := client.CallToolRaw("email.folder_delete", map[string]any{"name": name, "confirm": true})
		if err == nil || !strings.Contains(err.Error(), "protected") {
			t.Errorf("folder_delete(%q) = %v, want the protection refusal", name, err)
		}
	}
}

func TestFolderCreateRejectsNamesThatEscapeTheMailbox(t *testing.T) {
	client, _ := newClient(t)

	for _, name := range []string{"../escape", "a/b", ".hidden"} {
		_, err := client.CallToolRaw("email.folder_create", map[string]any{"name": name, "confirm": true})
		if err == nil || !strings.Contains(err.Error(), "invalid folder") {
			t.Errorf("folder_create(%q) = %v, want the name refused", name, err)
		}
	}
}

// A model has to know the deletion cannot destroy mail before it
// proposes one, and that is only true if the tool descriptions say so.
func TestFolderToolDescriptionsStateTheRefusals(t *testing.T) {
	client, _ := newClient(t)

	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	descriptions := map[string]string{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		descriptions[name], _ = tool["description"].(string)
	}

	create, ok := descriptions["email.folder_create"]
	if !ok {
		t.Fatal("email.folder_create is not registered")
	}
	for _, want := range []string{"Idempotent", "confirmation"} {
		if !strings.Contains(create, want) {
			t.Errorf("email.folder_create description missing %q", want)
		}
	}

	del, ok := descriptions["email.folder_delete"]
	if !ok {
		t.Fatal("email.folder_delete is not registered")
	}
	for _, want := range []string{"EMPTY", "never destroys mail", "how many", "no force flag", "confirmation"} {
		if !strings.Contains(del, want) {
			t.Errorf("email.folder_delete description missing %q", want)
		}
	}
}

// The instructions are the first thing a model reads, so the folder
// contract has to be in them too.
func TestInstructionsCoverFolderManagement(t *testing.T) {
	for _, want := range []string{"email.folder_create", "email.folder_delete", "EMPTY", "force flag"} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions missing %q", want)
		}
	}
}
