package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
	"github.com/pavel-alay/paprika-adapter/internal/syncer"
)

// verify exercises every read operation the adapter depends on against the
// configured account and prints a Markdown report. It reads only, stores nothing,
// and prints field names and counts, never values.
func verify(ctx context.Context) error {
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.store.Close()

	fmt.Printf("# Paprika protocol verification\n\n")
	fmt.Printf("- Run at: %s\n- Adapter: %s\n- Host: %s\n", stampUTC(time.Now()), version, a.client.Host())
	fmt.Printf("- Paprika app version: _fill in from the app's About screen_\n\n")
	fmt.Printf("| Operation | Request | Result | Rows | Field drift |\n|---|---|---|---|---|\n")

	failed := 0
	row := func(op, request string, rows int, drift paprika.Drift, err error) {
		result, count, d := "ok", "", "none"
		if rows >= 0 {
			count = fmt.Sprint(rows)
		}
		if !drift.Empty() {
			d = strings.TrimSpace(fmt.Sprintf("%s %s", list("new", drift.Unknown), list("gone", drift.Missing)))
		}
		if err != nil {
			failed++
			result, d = "FAILED: "+errText(err), ""
		}
		fmt.Printf("| %s | `%s` | %s | %s | %s |\n", op, request, result, count, d)
	}

	row("Login", "POST /api/v1/account/login/", -1, paprika.Drift{}, a.client.Login(ctx))

	counters, err := a.client.Status(ctx)
	var missing []string
	for _, c := range syncer.All() {
		if _, ok := counters[c]; err == nil && !ok {
			missing = append(missing, c)
		}
	}
	row("Change counters", "GET /api/v2/sync/status/", len(counters), paprika.Drift{Missing: missing}, err)

	refs, err := a.client.RecipeRefs(ctx)
	row("Recipe list", "GET /api/v2/sync/recipes/", len(refs), paprika.Drift{}, err)
	if len(refs) > 0 {
		rec, err := a.client.Recipe(ctx, refs[0].UID)
		row("Recipe detail", "GET /api/v2/sync/recipe/{uid}/", 1, paprika.DetectDrift(paprika.Recipes, []paprika.Raw{rec}), err)
	}
	for _, col := range paprika.Collections {
		rows, err := a.client.List(ctx, col)
		row(string(col), "GET /api/v2/sync/"+string(col)+"/", len(rows), paprika.DetectDrift(string(col), rows), err)
	}

	fmt.Printf("\nWrites are not exercised here. Verify them with the opt-in integration test (docs/RUNBOOK.md).\n")
	if failed > 0 {
		return fmt.Errorf("%d operation(s) failed verification", failed)
	}
	return nil
}

func list(label string, fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return label + ": " + strings.Join(fields, ", ")
}

func errText(err error) string {
	if k := paprika.KindOf(err); k != "" {
		return string(k) + " - " + err.Error()
	}
	return err.Error()
}
