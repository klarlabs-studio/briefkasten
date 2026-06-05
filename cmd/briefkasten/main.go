// Command briefkasten serves a mailbox as an MCP server.
//
// Configuration is resolved in 12-factor precedence — environment
// variables override the config file, which overrides defaults.
//
// Config file (BRIEFKASTEN_CONFIG, or ./briefkasten.yaml when present):
//
//	addr: ":8090"
//	backend: imap            # or maildir; inferred from imap.addr when omitted
//	maildir: ./maildir
//	imap:
//	  addr: imap.example.org:993
//	  username: alice
//	  password: "..."
//	  mailbox: INBOX
//	  insecure: false
//	runtime_config: false    # enable config.get / config.set MCP tools
//
// Environment overrides: BRIEFKASTEN_ADDR, BRIEFKASTEN_BACKEND,
// BRIEFKASTEN_MAILDIR, BRIEFKASTEN_IMAP_ADDR / _USER / _PASSWORD /
// _MAILBOX / _INSECURE, BRIEFKASTEN_RUNTIME_CONFIG.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/felixgeelhaar/bolt"
	mcp "github.com/felixgeelhaar/mcp-go"

	"github.com/felixgeelhaar/briefkasten"
)

func main() {
	log := bolt.New(bolt.NewJSONHandler(os.Stdout))

	cfg, err := loadConfig()
	if err != nil {
		log.Error().Err(err).Msg("config load failed")
		os.Exit(1)
	}

	srv, outbox, err := briefkasten.NewConfigServer(cfg)
	if err != nil {
		log.Error().Err(err).Msg("mailbox init failed")
		os.Exit(1)
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

	log.Info().
		Str("addr", cfg.Addr).
		Str("backend", cfg.ResolvedBackend()).
		Str("config_file", cfg.Path()).
		Bool("runtime_config", cfg.RuntimeConfig).
		Bool("outbox", outbox != nil).
		Msg("briefkasten listening")
	if err := mcp.ServeHTTP(ctx, srv, cfg.Addr); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("serve failed")
		os.Exit(1)
	}
}

// loadConfig reads BRIEFKASTEN_CONFIG, falls back to ./briefkasten.yaml
// when it exists, then overlays environment variables.
func loadConfig() (*briefkasten.Config, error) {
	path := os.Getenv("BRIEFKASTEN_CONFIG")
	if path == "" {
		if _, err := os.Stat("briefkasten.yaml"); err == nil {
			path = "briefkasten.yaml"
		}
	}
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.ApplyEnv()
	return cfg, nil
}
