package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
	"github.com/pavel-alay/paprika-adapter/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "with space", "..", "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func put(t *testing.T, s *store.Store, r map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(r)
	if err := s.PutRecipe(context.Background(), paprika.Raw{UID: r["uid"].(string), JSON: raw}, "H", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, s *store.Store, q store.SearchQuery) []string {
	t.Helper()
	q.Limit = 10
	raws, total, err := s.SearchRecipes(context.Background(), q)
	if err != nil {
		t.Fatalf("search %+v: %v", q, err)
	}
	if total != len(raws) {
		t.Fatalf("total = %d, rows = %d", total, len(raws))
	}
	var out []string
	for _, raw := range raws {
		r, _ := paprika.Decode[paprika.Recipe](raw)
		out = append(out, r.Name)
	}
	return out
}

func TestSearch(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.ReplaceEntities(ctx, paprika.Categories, []paprika.Raw{
		{UID: "C1", JSON: []byte(`{"uid":"C1","name":"Weeknight Dinner"}`)},
		{UID: "C2", JSON: []byte(`{"uid":"C2","name":"Dessert"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	put(t, s, map[string]any{"uid": "1", "name": "Chicken Soup", "ingredients": "1 chicken\n2 carrots", "categories": []string{"C1"}})
	put(t, s, map[string]any{"uid": "2", "name": "Carrot Cake", "ingredients": "3 carrots\n2 cups flour", "categories": []string{"C2"}})
	put(t, s, map[string]any{"uid": "3", "name": "Crème Brûlée", "ingredients": "cream\nsugar", "categories": []string{"C2"}, "notes": "torch the top"})
	put(t, s, map[string]any{"uid": "4", "name": "Binned Chicken", "ingredients": "chicken", "in_trash": true})
	if err := s.RebuildSearchIndex(ctx); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		q    store.SearchQuery
		want []string
	}{
		{"name outranks ingredient", store.SearchQuery{Text: "carrot"}, []string{"Carrot Cake", "Chicken Soup"}},
		{"all words must match", store.SearchQuery{Text: "carrot flour"}, []string{"Carrot Cake"}},
		{"prefix", store.SearchQuery{Text: "chick"}, []string{"Chicken Soup"}},
		{"diacritics folded", store.SearchQuery{Text: "creme brulee"}, []string{"Crème Brûlée"}},
		{"notes are searched", store.SearchQuery{Text: "torch"}, []string{"Crème Brûlée"}},
		{"category filter", store.SearchQuery{Category: "dessert"}, []string{"Carrot Cake", "Crème Brûlée"}},
		{"text and category", store.SearchQuery{Text: "carrot", Category: "dinner"}, []string{"Chicken Soup"}},
		{"trash on request", store.SearchQuery{Text: "binned", IncludeTrashed: true}, []string{"Binned Chicken"}},
		{"empty lists by name", store.SearchQuery{}, []string{"Carrot Cake", "Chicken Soup", "Crème Brûlée"}},
		{"fts syntax is inert", store.SearchQuery{Text: `"carrot" OR NOT (name: *`}, nil},
		{"punctuation only", store.SearchQuery{Text: `"*:()`}, []string{"Carrot Cake", "Chicken Soup", "Crème Brûlée"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := names(t, s, tc.q)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestRecipeRawTextSurvivesByteForByte(t *testing.T) {
	s := open(t)
	raw := `{"uid":"1","name":"X","ingredients":"1½ cups  flour \n\n  For the glaze:\n2 tbsp \"good\" honey","future_field":{"a":[1,2]}}`
	if err := s.PutRecipe(context.Background(), paprika.Raw{UID: "1", JSON: []byte(raw)}, "H", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Recipe(context.Background(), "1")
	if err != nil || !ok {
		t.Fatalf("recipe: ok=%v err=%v", ok, err)
	}
	if string(got.Raw) != raw {
		t.Fatalf("raw changed:\n got %s\nwant %s", got.Raw, raw)
	}
}

func TestCacheFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.sqlite")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReplaceEntities(context.Background(), paprika.Pantry, []paprika.Raw{{UID: "1", JSON: []byte(`{"uid":"1"}`)}}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(path + "*")
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v, want no group or other access", filepath.Base(m), info.Mode().Perm())
		}
	}
	if len(matches) == 0 {
		t.Fatal("no cache file found")
	}
}

func TestProposalLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, ok, _ := s.Proposal(ctx, "P"); ok {
		t.Fatal("unexpected proposal")
	}
	if err := s.SaveProposal(ctx, store.Proposal{ID: "P", Kind: "k", Payload: []byte(`{"a":1}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.Proposal(ctx, "P")
	if err != nil || !ok || p.State != store.ProposalPending || !p.ExpiresAt.Equal(now.Add(time.Minute)) || string(p.Payload) != `{"a":1}` {
		t.Fatalf("proposal = %+v ok=%v err=%v", p, ok, err)
	}
	if err := s.CommitProposal(ctx, "P", []byte(`{"done":true}`)); err != nil {
		t.Fatal(err)
	}
	if p, _, _ = s.Proposal(ctx, "P"); p.State != store.ProposalCommitted || string(p.Result) != `{"done":true}` {
		t.Fatalf("committed proposal = %+v", p)
	}
}
