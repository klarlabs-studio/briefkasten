package briefkasten

import (
	"context"
	"errors"
	"fmt"

	mcp "github.com/felixgeelhaar/mcp-go"
	"github.com/felixgeelhaar/mcp-go/server"
)

// confirmCuration puts a human in the loop before a destructive operation.
// Preferred path: MCP elicitation — the host prompts the user and only an
// explicit accept proceeds. Fallback for clients without elicitation: the
// caller must pass confirm=true, which the tool descriptions instruct
// agents to do only after asking the user.
func confirmCuration(ctx context.Context, confirmed bool, action, id string) error {
	if confirmed {
		return nil
	}
	session := server.SessionFromContext(ctx)
	if session != nil {
		result, err := server.NewElicitor(session).Elicit(ctx, &server.ElicitRequest{
			Message: fmt.Sprintf("Confirm %s of message %q? The message is moved, never destroyed.", action, id),
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		})
		if err == nil {
			if result.Action == "accept" {
				return nil
			}
			return fmt.Errorf("briefkasten: %s of %q declined by user", action, id)
		}
		// Elicitation unsupported by this client: fall through to the
		// confirm-flag contract.
	}
	return errors.New("briefkasten: confirmation required — ask the user, then retry with confirm=true")
}

// registerCurateTools exposes archive and delete with human-in-the-loop
// confirmation. Both are soft: nothing is ever expunged.
func registerCurateTools(srv *mcp.Server, resolve func(account, folder string) (Mailbox, error)) {
	srv.Tool("email.archive").
		Description("Archive an unread message (soft: filed away, never destroyed). Requires human confirmation — the host is asked via elicitation, or pass confirm=true after asking the user yourself.").
		Destructive().
		OutputSchema(map[string]any{"ok": true}).
		Handler(func(ctx context.Context, in struct {
			ID      string `json:"id"`
			Folder  string `json:"folder,omitempty"`
			Account string `json:"account,omitempty"`
			Confirm bool   `json:"confirm,omitempty"`
		}) (map[string]any, error) {
			if err := confirmCuration(ctx, in.Confirm, "archive", in.ID); err != nil {
				return nil, err
			}
			cu, err := curatorFor(resolve, in.Account, in.Folder)
			if err != nil {
				return nil, err
			}
			if err := cu.Archive(in.ID); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})

	srv.Tool("email.delete").
		Description("Move an unread message to trash (soft delete: never expunged). Requires human confirmation — the host is asked via elicitation, or pass confirm=true after asking the user yourself.").
		Destructive().
		OutputSchema(map[string]any{"ok": true}).
		Handler(func(ctx context.Context, in struct {
			ID      string `json:"id"`
			Folder  string `json:"folder,omitempty"`
			Account string `json:"account,omitempty"`
			Confirm bool   `json:"confirm,omitempty"`
		}) (map[string]any, error) {
			if err := confirmCuration(ctx, in.Confirm, "delete", in.ID); err != nil {
				return nil, err
			}
			cu, err := curatorFor(resolve, in.Account, in.Folder)
			if err != nil {
				return nil, err
			}
			if err := cu.Delete(in.ID); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})
}

// curatorFor resolves the curation target through account and folder
// routing, then asserts the capability.
func curatorFor(resolve func(account, folder string) (Mailbox, error), account, folder string) (Curator, error) {
	box, err := resolve(account, folder)
	if err != nil {
		return nil, err
	}
	cu, ok := box.(Curator)
	if !ok {
		return nil, errors.New("briefkasten: backend has no curation support")
	}
	return cu, nil
}
