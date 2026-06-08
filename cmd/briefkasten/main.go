// Command briefkasten serves a mailbox as an MCP server — and doubles as
// a human CLI over the same mailbox.
//
// Server (default):
//
//	briefkasten [serve]
//
// Human commands:
//
//	briefkasten list   [--folder F] [--account A] [--json]
//	briefkasten read   <id>
//	briefkasten seen   <id>
//	briefkasten search <query>
//	briefkasten folders
//	briefkasten send   --to a@b.c --subject S --body B
//	briefkasten archive <id>   (prompts; --yes to skip)
//	briefkasten delete  <id>   (prompts; --yes to skip — soft delete, to trash)
//
// Configuration: briefkasten.yaml / BRIEFKASTEN_CONFIG / BRIEFKASTEN_* env;
// see the README for the full reference.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.klarlabs.de/bolt"
	mcp "go.klarlabs.de/mcp"

	"go.klarlabs.de/briefkasten"
)

// Build metadata, injected by goreleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("briefkasten %s (commit: %s, built: %s)\n", version, commit, date)
		return
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func contextTODO() context.Context { return context.Background() }

// serve runs the MCP server (the pre-CLI default behavior).
func serve() int {
	log := bolt.New(bolt.NewJSONHandler(os.Stdout))

	cfg, err := loadConfigPath("")
	if err != nil {
		log.Error().Err(err).Msg("config load failed")
		return 1
	}

	srv, outbox, err := briefkasten.NewConfigServer(cfg)
	if err != nil {
		log.Error().Err(err).Msg("mailbox init failed")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Outbox worker: deliver the queued backlog continuously.
	if outbox != nil {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if n, err := outbox.ProcessOnce(ctx); err != nil {
						log.Error().Err(err).Msg("outbox process failed")
					} else if n > 0 {
						log.Info().Int("delivered", n).Msg("outbox delivered")
					}
				}
			}
		}()
	}

	// New-mail watcher: push notifications/resources/updated to subscribers of
	// email://inbox instead of making them poll. Best-effort — the mailbox
	// stays pollable if watching fails or is unsupported.
	if watcher := cfg.BuildWatcher(); watcher != nil {
		go func() {
			err := watcher.Watch(ctx, func() {
				if nerr := srv.NotifyResourceUpdated(briefkasten.InboxResourceURI); nerr != nil {
					log.Debug().Err(nerr).Msg("inbox update notify failed")
				}
			})
			if err != nil && ctx.Err() == nil {
				log.Warn().Err(err).Msg("inbox watcher stopped; subscribers fall back to polling")
			}
		}()
	}

	log.Info().
		Str("addr", cfg.Addr).
		Str("backend", cfg.ResolvedBackend()).
		Str("config_file", cfg.Path()).
		Bool("runtime_config", cfg.RuntimeConfig).
		Bool("outbox", outbox != nil).
		Msg("briefkasten listening")
	if err := mcp.ServeHTTP(ctx, srv, cfg.Addr); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("serve failed")
		return 1
	}
	return 0
}
