package syncer_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/paprikatest"
	"paprika-adapter/internal/store"
	"paprika-adapter/internal/syncer"
)

type harness struct {
	fake  *paprikatest.Server
	store *store.Store
	sync  *syncer.Syncer
	now   time.Time
}

func newHarness(t *testing.T, password string) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	h.fake = paprikatest.New(h.now)
	client, err := paprika.New(paprika.Options{BaseURL: h.fake.URL, Email: paprikatest.Email, Password: password,
		HTTPClient: h.fake.Client(), RetryWait: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	h.store, err = store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.store.Close() })
	h.sync = syncer.New(client, h.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.sync.Now = func() time.Time { return h.now }
	return h
}

func (h *harness) refresh(t *testing.T) map[string]syncer.Freshness {
	t.Helper()
	out := map[string]syncer.Freshness{}
	for _, f := range h.sync.Refresh(context.Background(), syncer.All()...) {
		out[f.Collection] = f
	}
	return out
}

func (h *harness) search(t *testing.T, text string) int {
	t.Helper()
	_, total, err := h.store.SearchRecipes(context.Background(), store.SearchQuery{Text: text, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func TestFirstSyncFillsEveryCollection(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	report := h.refresh(t)
	want := map[string]int{"recipes": 6, "categories": 5, "pantry": 8, "meals": 5, "mealtypes": 4,
		"grocerylists": 2, "groceries": 5, "groceryaisles": 6, "groceryingredients": 5}
	for col, rows := range want {
		f := report[col]
		if f.State != syncer.StateFresh || f.Rows != rows || f.Error != nil || f.Drift != nil {
			t.Errorf("%s = %+v, want fresh with %d rows and no error or drift", col, f, rows)
		}
	}
	if len(report) != len(want) {
		t.Errorf("collections reported = %d, want %d", len(report), len(want))
	}
}

func TestUnchangedAccountCostsOneRequest(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.refresh(t)
	before := h.fake.Hits("GET ")
	h.refresh(t)
	if extra := h.fake.Hits("GET ") - before; extra != 1 {
		t.Fatalf("second sync made %d requests, want 1 (the status check)", extra)
	}
}

func TestOnlyChangedRecipeIsFetched(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.refresh(t)
	if h.search(t, "saffron") != 0 {
		t.Fatal("fixture unexpectedly matches saffron")
	}
	h.fake.UpdateRecipe("014031E3-BC74-4CA8-B345-36EBE9C9794B", "notes", "add a pinch of saffron")
	before := h.fake.Hits("GET /api/v2/sync/recipe/")
	h.refresh(t)
	if fetched := h.fake.Hits("GET /api/v2/sync/recipe/") - before; fetched != 1 {
		t.Fatalf("recipes fetched = %d, want 1", fetched)
	}
	if h.search(t, "saffron") != 1 {
		t.Fatal("search index was not updated with the edited recipe")
	}
}

func TestRecipeDeletedInPaprikaLeavesTheCache(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.refresh(t)
	h.fake.RemoveRecipe("7F2C6A10-D3B8-4E59-8A41-9B0E7C5D2F16")
	if f := h.refresh(t)["recipes"]; f.Rows != 5 {
		t.Fatalf("recipes cached = %d, want 5", f.Rows)
	}
	if h.search(t, "pancakes") != 0 {
		t.Fatal("deleted recipe is still searchable")
	}
}

func TestOnlyChangedCollectionIsRefetched(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.refresh(t)
	h.fake.Put("pantry", map[string]any{"uid": "NEW-PANTRY-ITEM", "ingredient": "Paprika", "in_stock": true})
	before, beforePantry := h.fake.Hits("GET "), h.fake.Hits("GET /api/v2/sync/pantry/")
	report := h.refresh(t)
	if h.fake.Hits("GET /api/v2/sync/pantry/")-beforePantry != 1 || h.fake.Hits("GET ")-before != 2 {
		t.Fatalf("requests = %d, want status + pantry only", h.fake.Hits("GET ")-before)
	}
	if report["pantry"].Rows != 9 {
		t.Fatalf("pantry rows = %d, want 9", report["pantry"].Rows)
	}
}

func TestCountersAreNotTrustedForever(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.refresh(t)
	h.now = h.now.Add(h.sync.CounterTrust + time.Minute)
	before := h.fake.Hits("GET /api/v2/sync/meals/")
	h.refresh(t)
	if h.fake.Hits("GET /api/v2/sync/meals/")-before != 1 {
		t.Fatal("meals were not re-downloaded after the counter trust window")
	}
}

func TestOutageKeepsCacheAndReportsStale(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.refresh(t)
	h.fake.SetDown(true)
	h.now = h.now.Add(time.Hour)
	f := h.refresh(t)["meals"]
	if f.State != syncer.StateStale || f.Rows != 5 || f.Error == nil || f.Error.Code != "upstream_unavailable" {
		t.Fatalf("meals = %+v (error %+v), want stale, 5 rows, upstream_unavailable", f, f.Error)
	}
	if f.AgeSeconds == nil || *f.AgeSeconds != 3600 {
		t.Fatalf("age = %v, want 3600", f.AgeSeconds)
	}

	h.fake.SetDown(false)
	if f := h.refresh(t)["meals"]; f.State != syncer.StateFresh || f.Error != nil {
		t.Fatalf("meals after recovery = %+v, want fresh without error", f)
	}
}

func TestEnsureSkipsFreshDataAndBacksOffAfterFailure(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	ctx := context.Background()
	h.sync.Ensure(ctx, "meals")
	before := h.fake.Hits("GET ")
	h.sync.Ensure(ctx, "meals")
	if h.fake.Hits("GET ") != before {
		t.Fatal("Ensure contacted Paprika although the data was fresh")
	}

	h.fake.SetDown(true)
	h.now = h.now.Add(h.sync.MaxAge + time.Second)
	h.sync.Ensure(ctx, "meals")
	afterFailure := h.fake.Hits("GET ")
	f := h.sync.Ensure(ctx, "meals")[0]
	if h.fake.Hits("GET ") != afterFailure {
		t.Fatal("Ensure retried immediately after a failure")
	}
	if f.State != syncer.StateStale || f.Error == nil {
		t.Fatalf("meals = %+v, want stale with the recorded error", f)
	}
}

func TestRejectedCredentialsAreReportedAsAuthFailure(t *testing.T) {
	h := newHarness(t, "wrong-password")
	f := h.refresh(t)["recipes"]
	if f.State != syncer.StateEmpty || f.Error == nil || f.Error.Code != "auth_failed" {
		t.Fatalf("recipes = %+v (error %+v), want never_synced with auth_failed", f, f.Error)
	}
}

func TestInterruptedRecipeSyncResumes(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.sync.RecipeWorkers = 1
	h.fake.FailNext("GET /api/v2/sync/recipe/3B9E1D42-6A7C-4F85-9D20-1C8E5A4B7F63/", 500, 500, 500)
	f := h.refresh(t)["recipes"]
	if f.Rows != 5 || f.Error == nil || f.State == syncer.StateFresh {
		t.Fatalf("recipes = %+v (error %+v), want 5 cached and an error", f, f.Error)
	}
	before := h.fake.Hits("GET /api/v2/sync/recipe/")
	f = h.refresh(t)["recipes"]
	if f.Rows != 6 || f.Error != nil || f.State != syncer.StateFresh {
		t.Fatalf("recipes after retry = %+v, want 6 and fresh", f)
	}
	if fetched := h.fake.Hits("GET /api/v2/sync/recipe/") - before; fetched != 1 {
		t.Fatalf("retry fetched %d recipes, want only the missing one", fetched)
	}
}

func TestNewFieldFromPaprikaIsReportedAsDriftAndKept(t *testing.T) {
	h := newHarness(t, paprikatest.Password)
	h.fake.Put("grocerylists", map[string]any{"uid": "X", "name": "New", "order_flag": 9, "is_default": false,
		"reminders_list": "Paprika", "shared_with": []string{"someone"}})
	f := h.refresh(t)["grocerylists"]
	if f.Drift == nil || len(f.Drift.Unknown) != 1 || f.Drift.Unknown[0] != "shared_with" {
		t.Fatalf("drift = %+v, want unknown=[shared_with]", f.Drift)
	}
	raws, err := h.store.Entities(context.Background(), paprika.GroceryLists)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range raws {
		if strings.Contains(string(r), `"shared_with"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("unknown field was dropped from the cached row")
	}
}
