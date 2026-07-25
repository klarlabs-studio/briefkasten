// Command briefkasten serves a mailbox as an MCP server — and doubles as
// a human CLI over the same mailbox.
//
// Server (default):
//
//	briefkasten [serve] [--stdio] [--config FILE]
//
// The server speaks MCP over HTTP by default; --stdio (or
// transport: stdio / BRIEFKASTEN_TRANSPORT=stdio) serves JSON-RPC over
// stdin/stdout for hosts that spawn the binary as a child process.
//
// Human commands:
//
//	briefkasten list   [--folder F] [--account A] [--json]
//	briefkasten read   <id>
//	briefkasten seen   <id>
//	briefkasten search <query>
//	briefkasten folders
//	briefkasten send   --to a@b.c --subject S --body B
//	briefkasten retry  <id>    (re-queue + deliver a failed send)
//	briefkasten outbox         (outbound ids by lifecycle state)
//	briefkasten archive <id>   (prompts; --yes to skip)
//	briefkasten delete  <id>   (prompts; --yes to skip — soft delete, to trash)
//	briefkasten --version      (or "version"; --json for build metadata)
//
// Configuration: briefkasten.yaml / BRIEFKASTEN_CONFIG / BRIEFKASTEN_* env;
// see the README for the full reference.
package main

import (
	"context"
	"flag"
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
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func contextTODO() context.Context { return context.Background() }

// serveOptions are the flags accepted by the serve command.
type serveOptions struct {
	configPath string
	stdio      bool
}

// parseServeFlags parses the serve command's flags. --stdio is a
// shorthand for transport: stdio, so clients that spawn the binary can
// select it without a config file or environment variable.
func parseServeFlags(args []string) (serveOptions, error) {
	var opts serveOptions
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.configPath, "config", "", "config file (default: $BRIEFKASTEN_CONFIG or ./briefkasten.yaml)")
	fs.BoolVar(&opts.stdio, "stdio", false, "serve MCP over stdin/stdout instead of HTTP")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	return opts, nil
}

// serve runs the MCP server (the pre-CLI default behavior).
func serve(args []string) int {
	// Bootstrap logger on stderr: the transport is not known until the
	// config is read, and stdio mode must never write to stdout.
	log := bolt.New(bolt.NewJSONHandler(os.Stderr))

	opts, err := parseServeFlags(args)
	if err != nil {
		return 2
	}

	cfg, err := loadConfigPath(opts.configPath)
	if err != nil {
		log.Error().Err(err).Msg("config load failed")
		return 1
	}
	// The flag is the most explicit signal, so it wins over file and env.
	if opts.stdio {
		cfg.Transport = briefkasten.TransportStdio
	}
	if err := cfg.ValidateTransport(); err != nil {
		log.Error().Err(err).Msg("transport invalid")
		return 1
	}
	// HTTP keeps logging to stdout as before; stdio stays on stderr.
	log = bolt.New(bolt.NewJSONHandler(cfg.LogWriter()))

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

	authMW, err := cfg.BuildAuthMiddleware()
	if err != nil {
		log.Error().Err(err).Msg("auth init failed")
		return 1
	}

	transport := cfg.ResolvedTransport()
	entry := log.Info().
		Str("transport", transport).
		Str("backend", cfg.ResolvedBackend()).
		Str("config_file", cfg.Path()).
		Bool("runtime_config", cfg.RuntimeConfig).
		Bool("outbox", outbox != nil)

	if transport == briefkasten.TransportStdio {
		// Basic auth guards an HTTP endpoint; over stdio the peer is the
		// process that spawned us, so there is no credential to check.
		if authMW != nil {
			log.Warn().Msg("auth configured but ignored on stdio transport")
		}
		entry.Msg("briefkasten serving on stdio")
		err = mcp.ServeStdio(ctx, srv)
	} else {
		entry.Str("addr", cfg.Addr).Bool("auth", authMW != nil).Msg("briefkasten listening")
		if authMW != nil {
			err = mcp.ServeHTTPWithMiddleware(ctx, srv, cfg.Addr,
				[]mcp.HTTPOption{briefkasten.ForwardAuthorizationHeader()},
				mcp.WithMiddleware(authMW))
		} else {
			err = mcp.ServeHTTP(ctx, srv, cfg.Addr)
		}
	}
	if err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("serve failed")
		return 1
	}
	return 0
}
