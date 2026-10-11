package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
	"github.com/pavel-alay/paprika-adapter/internal/syncer"
)

// export writes everything in the cache, exactly as Paprika returned it, into a
// new timestamped directory under DIR, or as a tar stream on stdout when DIR is
// "-" (the form that works from a container without a bind mount). It is a copy
// of the cache, not a replacement for Paprika's own export.
func export(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	offline := fs.Bool("offline", false, "export the cache as it is, without contacting Paprika first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: paprika-adapter export [-offline] DIR|-")
	}
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.store.Close()

	now := time.Now()
	freshness := a.sync.Freshness(ctx, syncer.All()...)
	if !*offline {
		freshness = a.sync.Refresh(ctx, syncer.All()...)
	}
	name := "paprika-export-" + now.UTC().Format("20060102T150405Z")
	toStdout := fs.Arg(0) == "-"
	parent := fs.Arg(0)
	if toStdout {
		// Staged inside the data directory: it is the one place known to be writable.
		parent = filepath.Join(a.cfg.dataDir, "export-staging")
		defer os.RemoveAll(parent)
	}
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	recipes, err := a.store.AllRecipes(ctx)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "recipes.json"), recipes); err != nil {
		return err
	}
	for _, col := range paprika.Collections {
		rows, err := a.store.Entities(ctx, col)
		if err != nil {
			return err
		}
		if rows == nil {
			rows = []json.RawMessage{}
		}
		if err := writeJSON(filepath.Join(dir, string(col)+".json"), rows); err != nil {
			return err
		}
	}
	if err := a.store.Snapshot(ctx, filepath.Join(dir, "cache.sqlite")); err != nil {
		return err
	}
	manifest := map[string]any{
		"exported_at": stampUTC(now), "adapter_version": version, "paprika_host": a.client.Host(),
		"note":        "Copy of the adapter cache. Rows are verbatim Paprika sync API objects. Check each collection's state before relying on it.",
		"collections": freshness,
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return err
	}
	if toStdout {
		if err := writeTar(os.Stdout, parent); err != nil {
			return err
		}
	} else {
		fmt.Println(dir)
	}
	for _, f := range freshness {
		if f.State != syncer.StateFresh {
			return fmt.Errorf("export written, but %s is %s; see manifest.json", f.Collection, f.State)
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// writeTar streams everything under parent as a tar archive.
func writeTar(w io.Writer, parent string) error {
	tw := tar.NewWriter(w)
	if err := tw.AddFS(os.DirFS(parent)); err != nil {
		return err
	}
	return tw.Close()
}
