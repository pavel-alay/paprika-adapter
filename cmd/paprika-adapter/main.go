// Command paprika-adapter serves one household's Paprika 3 account to an MCP
// client over stdio, backed by a local SQLite cache.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // the container image has no zoneinfo

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"paprika-adapter/internal/mcpserver"
	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
	"paprika-adapter/internal/syncer"
)

var version = "dev"

const usage = `paprika-adapter %s

Usage: paprika-adapter [command]

  serve     run the MCP server on stdin/stdout (default)
  sync      pull everything from Paprika into the cache once, print the result
  verify    probe every Paprika operation the adapter relies on, print a report
  export    write the cache as JSON plus a SQLite snapshot:  export [-offline] DIR   (DIR "-" = tar on stdout)
  tools     print the MCP tool schemas as JSON
  version   print the version

Configuration is read from the environment, see docs/INSTALL.md.
`

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "sync":
		err = syncOnce(ctx)
	case "verify":
		err = verify(ctx)
	case "export":
		err = export(ctx, os.Args[2:])
	case "tools":
		err = printTools(ctx, os.Stdout)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "paprika-adapter:", err)
		os.Exit(1)
	}
}

// app is the wiring shared by every command that touches Paprika or the cache.
type app struct {
	cfg    config
	log    *slog.Logger
	client *paprika.Client
	store  *store.Store
	sync   *syncer.Syncer
}

func newApp(ctx context.Context) (*app, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.logLevel}))
	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}
	client, err := paprika.New(paprika.Options{
		BaseURL: cfg.baseURL, Email: cfg.email, Password: cfg.password,
		TokenFile: filepath.Join(cfg.dataDir, "token"), UserAgent: "paprika-adapter/" + version, Logger: log,
	})
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.dataDir, "cache.sqlite"))
	if err != nil {
		return nil, err
	}
	sy := syncer.New(client, st, log)
	sy.MaxAge = cfg.maxAge
	return &app{cfg: cfg, log: log, client: client, store: st, sync: sy}, nil
}

func (a *app) deps() mcpserver.Deps {
	return mcpserver.Deps{
		Store: a.store, Sync: a.sync, Client: a.client, WritesEnabled: a.cfg.writes,
		Location: a.cfg.location, Version: version, RequireHumanApproval: a.cfg.humanOnly,
	}
}

func serve(ctx context.Context) error {
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.store.Close()
	a.log.Info("paprika-adapter starting", "version", version, "writes_enabled", a.cfg.writes,
		"paprika_host", a.client.Host(), "data_dir", a.cfg.dataDir)
	if a.cfg.syncInterval > 0 {
		go a.sync.Run(ctx, a.cfg.syncInterval)
	}
	err = mcpserver.New(a.deps()).Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func syncOnce(ctx context.Context) error {
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.store.Close()
	report := a.sync.Refresh(ctx, syncer.All()...)
	if err := printJSON(report); err != nil {
		return err
	}
	for _, f := range report {
		if f.Error != nil {
			return fmt.Errorf("sync incomplete: %s: %s", f.Collection, f.Error.Message)
		}
	}
	return nil
}

// printTools lists the tools exactly as an MCP client sees them, write tools included.
func printTools(ctx context.Context, w io.Writer) error {
	srvT, cliT := mcp.NewInMemoryTransports()
	srv := mcpserver.New(mcpserver.Deps{WritesEnabled: true, Version: version})
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		return err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "paprika-adapter-tools", Version: version}, nil).Connect(ctx, cliT, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	return encodeJSON(w, res.Tools)
}

func printJSON(v any) error { return encodeJSON(os.Stdout, v) }

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func stampUTC(t time.Time) string { return t.UTC().Format(time.RFC3339) }
