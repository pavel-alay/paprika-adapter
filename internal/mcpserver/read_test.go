package mcpserver_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pavel-alay/paprika-adapter/internal/paprikatest"
	"github.com/pavel-alay/paprika-adapter/internal/syncer"
)

func toolNames(t *testing.T, h *harness) []string {
	t.Helper()
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %s lacks a description or a schema", tool.Name)
		}
	}
	sort.Strings(names)
	return names
}

func TestReadOnlyServerExposesNoWriteTools(t *testing.T) {
	got := strings.Join(toolNames(t, newHarness(t, false)), " ")
	want := "get_meal_plan get_recipe get_recipe_photo list_grocery_items list_grocery_lists list_pantry search_recipes sync_status"
	if got != want {
		t.Fatalf("tools = %s\n want  %s", got, want)
	}
}

func TestWriteServerAddsOnlyProposeAndCommit(t *testing.T) {
	names := toolNames(t, newHarness(t, true))
	if len(names) != 16 {
		t.Fatalf("tools = %v, want 16", names)
	}
	// Four tools change existing data, each asked for by the household: the recipe edit
	// (rating, notes, description, directions), the meal-plan edit (day, meal type,
	// what is planned), the pantry edit and the removal of pantry items. Nothing else
	// deletes or edits.
	asked := map[string]bool{"propose_recipe_edit": true, "propose_meal_plan_edit": true, "propose_pantry_edit": true, "propose_pantry_removal": true}
	for _, n := range names {
		if asked[n] {
			continue
		}
		for _, banned := range []string{"delete", "remov", "update", "edit", "clear", "bulk"} {
			if strings.Contains(n, banned) {
				t.Errorf("must not expose %s", n)
			}
		}
	}
}

func TestSearchRecipes(t *testing.T) {
	h := newHarness(t, false)
	r := h.ok("search_recipes", map[string]any{"query": "chicken"})
	if r.TotalMatches != 2 || r.Truncated || len(r.Recipes) != 2 {
		t.Fatalf("chicken: %+v", r.Recipes)
	}
	for _, rec := range r.Recipes {
		if rec.UID == trashedUID {
			t.Error("trashed recipe returned by default")
		}
	}
	if len(r.Sync) != 2 || r.Sync[0].State != syncer.StateFresh || r.Sync[0].LastSyncedAt != "2026-10-08T12:00:00Z" {
		t.Fatalf("sync block = %+v", r.Sync)
	}

	r = h.ok("search_recipes", map[string]any{"query": "lentil"})
	if len(r.Recipes) != 1 || r.Recipes[0].UID != soupUID || strings.Join(r.Recipes[0].Categories, ",") != "Soup,Vegetarian,Dinner" {
		t.Fatalf("lentil: %+v", r.Recipes)
	}
	if r = h.ok("search_recipes", map[string]any{"category": "breakfast"}); len(r.Recipes) != 1 || r.Recipes[0].Name != "Buttermilk Pancakes" {
		t.Fatalf("category breakfast: %+v", r.Recipes)
	}
	if r = h.ok("search_recipes", map[string]any{"limit": 2}); len(r.Recipes) != 2 || r.TotalMatches != 5 || !r.Truncated {
		t.Fatalf("limit 2: %d of %d truncated=%v", len(r.Recipes), r.TotalMatches, r.Truncated)
	}
	if r = h.ok("search_recipes", map[string]any{"query": "tuna", "include_trashed": true}); len(r.Recipes) != 1 || !r.Recipes[0].InTrash {
		t.Fatalf("trashed on request: %+v", r.Recipes)
	}
	if r = h.ok("search_recipes", map[string]any{"query": "zzzz"}); r.Recipes == nil || len(r.Recipes) != 0 {
		t.Fatalf("no match must be an empty list, got %+v", r.Recipes)
	}
}

func TestGetRecipeKeepsRawIngredientTextBesideParsedLines(t *testing.T) {
	h := newHarness(t, false)
	rec := h.ok("get_recipe", map[string]any{"uid": roastUID}).Recipe
	wantRaw := "1 whole chicken (about 4 lb)\n2 lemons, halved\n1 head garlic, halved crosswise\n3 tbsp olive oil\n" +
		"1 1/2 tsp kosher salt\n½ tsp black pepper\nFor the pan:\n1 lb baby potatoes\n2 carrots, cut into chunks"
	if rec.IngredientsRaw != wantRaw {
		t.Fatalf("raw ingredients changed:\n%q", rec.IngredientsRaw)
	}
	var rebuilt []string
	for _, l := range rec.IngredientsParsed {
		rebuilt = append(rebuilt, l.Raw)
	}
	if strings.Join(rebuilt, "\n") != wantRaw {
		t.Fatalf("parsed lines do not carry the raw text: %q", rebuilt)
	}
	p := rec.IngredientsParsed
	if p[4].Quantity != "1 1/2" || p[4].Unit != "tsp" || p[4].Item != "kosher salt" {
		t.Errorf("line 5 parsed as %+v", p[4])
	}
	if p[1].Quantity != "2" || p[1].Unit != "" || p[1].Item != "lemons" || p[1].Note != "halved" {
		t.Errorf("line 2 parsed as %+v", p[1])
	}
	if p[5].Quantity != "½" || p[5].Unit != "tsp" || !p[6].Heading {
		t.Errorf("fraction or heading not recognised: %+v %+v", p[5], p[6])
	}
	if len(rec.Categories) != 1 || rec.Categories[0].Name != "Dinner" || rec.CachedAt != "2026-10-08T12:00:00Z" {
		t.Errorf("categories or cached_at wrong: %+v %s", rec.Categories, rec.CachedAt)
	}

	h.fails("get_recipe", map[string]any{"uid": "NOPE"}, "not_found")
	h.fails("get_recipe", map[string]any{"uid": "  "}, "invalid_input")
}

func TestListPantry(t *testing.T) {
	h := newHarness(t, false)
	r := h.ok("list_pantry", nil)
	if r.Total != 8 || len(r.Items) != 8 || r.Items[0].Ingredient != "Bread flour" {
		t.Fatalf("pantry: total=%d first=%+v", r.Total, r.Items[0])
	}
	if r = h.ok("list_pantry", map[string]any{"in_stock_only": true}); r.Total != 7 {
		t.Fatalf("in stock = %d, want 7", r.Total)
	}
	r = h.ok("list_pantry", map[string]any{"query": "STOCK"})
	if len(r.Items) != 1 || r.Items[0].Ingredient != "Vegetable stock" || r.Items[0].ExpirationDate != "2027-01-31" {
		t.Fatalf("query stock: %+v", r.Items)
	}
}

func TestMealPlanDefaultsToCurrentWeek(t *testing.T) {
	h := newHarness(t, false)
	r := h.ok("get_meal_plan", nil)
	if r.From != "2026-10-05" || r.To != "2026-10-11" || r.Today != "2026-10-08" {
		t.Fatalf("range = %s..%s today %s", r.From, r.To, r.Today)
	}
	var got []string
	for _, m := range r.Meals {
		got = append(got, m.Date+" "+m.MealType+" "+m.Name)
	}
	want := []string{
		"2026-10-05 Dinner Red Lentil Soup", "2026-10-06 Lunch Leftover soup",
		"2026-10-07 Dinner Weeknight Chicken Stir-Fry", "2026-10-10 Breakfast Buttermilk Pancakes",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("meals = %v\n want  %v", got, want)
	}
	if r.Meals[0].RecipeUID != soupUID || r.Meals[1].RecipeUID != "" || len(r.MealTypes) != 4 || r.MealTypes[2].UID != dinner {
		t.Fatalf("recipe links or meal types wrong: %+v %+v", r.Meals[:2], r.MealTypes)
	}

	r = h.ok("get_meal_plan", map[string]any{"from": "2026-09-28", "to": "2026-10-04"})
	if len(r.Meals) != 1 || r.Meals[0].Name != "Lemon Garlic Roast Chicken" {
		t.Fatalf("previous week: %+v", r.Meals)
	}
	h.fails("get_meal_plan", map[string]any{"from": "next monday"}, "invalid_input")
	h.fails("get_meal_plan", map[string]any{"from": "2026-10-05", "to": "2026-10-01"}, "invalid_input")
	h.fails("get_meal_plan", map[string]any{"from": "2026-01-01", "to": "2026-12-31"}, "invalid_input")
}

func TestGroceryListsAndItems(t *testing.T) {
	h := newHarness(t, false)
	lists := h.ok("list_grocery_lists", nil).Lists
	if len(lists) != 2 || lists[0].UID != defaultList || !lists[0].IsDefault || lists[0].Items != 4 || lists[0].Unpurchased != 3 || lists[1].Items != 1 {
		t.Fatalf("lists = %+v", lists)
	}
	r := h.ok("list_grocery_items", map[string]any{"list_uid": defaultList})
	if r.List.Name != "My Grocery List" || r.Total != 3 || r.Items[0].Name != "Milk" || r.Items[1].Quantity != "2" {
		t.Fatalf("items = %+v", r.Items)
	}
	if r = h.ok("list_grocery_items", map[string]any{"list_uid": defaultList, "include_purchased": true}); r.Total != 4 {
		t.Fatalf("with purchased = %d, want 4", r.Total)
	}
	h.fails("list_grocery_items", map[string]any{"list_uid": "NOPE"}, "not_found")
	h.fails("list_grocery_items", map[string]any{"list_uid": ""}, "invalid_input")
}

func TestOutageServesCacheMarkedStale(t *testing.T) {
	h := newHarness(t, false)
	h.ok("get_meal_plan", nil)
	h.fake.SetDown(true)
	h.now = h.now.Add(2 * time.Hour)

	r := h.ok("get_meal_plan", map[string]any{"from": "2026-10-05"})
	if len(r.Meals) != 4 {
		t.Fatalf("cached meals not served during outage: %+v", r.Meals)
	}
	f := r.Sync[0]
	if f.State != syncer.StateStale || f.Error == nil || f.Error.Code != "upstream_unavailable" || *f.AgeSeconds != 7200 {
		t.Fatalf("sync = %+v error=%+v, want stale + upstream_unavailable + age 7200", f, f.Error)
	}

	st := h.ok("sync_status", nil)
	if st.Healthy || len(st.Sync) != 10 {
		t.Fatalf("sync_status healthy=%v collections=%d", st.Healthy, len(st.Sync))
	}
}

func TestNothingCachedAndPaprikaDownIsAnError(t *testing.T) {
	h := newHarness(t, false)
	h.fake.SetDown(true)
	r := h.fails("list_pantry", nil, "upstream_unavailable")
	if !r.Error.Retryable || r.Sync[0].State != syncer.StateEmpty {
		t.Fatalf("error=%+v sync=%+v", r.Error, r.Sync)
	}
}

func TestSyncStatusRefresh(t *testing.T) {
	h := newHarness(t, true)
	if r := h.ok("sync_status", nil); r.Healthy || r.Sync[0].State != syncer.StateEmpty || !r.WritesEnabled {
		t.Fatalf("before any sync: healthy=%v %+v", r.Healthy, r.Sync[0])
	}
	if h.fake.Hits("GET ") != 0 {
		t.Fatal("sync_status without refresh contacted Paprika")
	}
	if r := h.ok("sync_status", map[string]any{"refresh": true}); !r.Healthy {
		t.Fatalf("after refresh: %+v", r.Sync)
	}
}

func TestSecretsNeverReachRepliesOrLogs(t *testing.T) {
	h := newHarness(t, true)
	h.ok("sync_status", map[string]any{"refresh": true})
	h.ok("search_recipes", nil)
	h.ok("get_recipe", map[string]any{"uid": roastUID})
	h.fake.RevokeToken()
	h.now = h.now.Add(time.Hour)
	h.ok("list_pantry", nil)
	p := h.ok("propose_grocery_items", map[string]any{"list_uid": defaultList, "items": []any{map[string]any{"name": "tea"}}})
	h.ok("commit_proposal", map[string]any{"proposal_id": p.Proposal.ProposalID, "confirm": true})
	h.fake.SetDown(true)
	h.now = h.now.Add(time.Hour)
	h.ok("sync_status", map[string]any{"refresh": true})
	h.call("list_grocery_lists", nil)

	if h.logs.Len() == 0 || h.replies.Len() == 0 {
		t.Fatal("nothing was captured")
	}
	for _, secret := range []string{paprikatest.Password, "fixture-token", "Bearer", paprikatest.Email} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("%q leaked into the logs", secret)
		}
		if strings.Contains(h.replies.String(), secret) {
			t.Errorf("%q leaked into a tool reply", secret)
		}
	}
}

// A first sync that fetched only some recipes must not be searched as if it
// were the whole collection: "no match" would be a wrong answer.
func TestIncompleteFirstSyncIsNotAnsweredFrom(t *testing.T) {
	h := newHarness(t, false)
	h.fake.FailNext("GET /api/v2/sync/recipe/"+roastUID+"/", 500, 500, 500)
	r := h.fails("search_recipes", map[string]any{"query": "lentil"}, "upstream_unavailable")
	if !r.Error.Retryable || len(r.Recipes) != 0 {
		t.Fatalf("error = %+v, recipes = %v", r.Error, r.Recipes)
	}
	for _, f := range r.Sync {
		if f.Collection == "recipes" && (f.State != syncer.StateEmpty || f.Rows != 5) {
			t.Fatalf("recipes = %+v, want never_synced with the 5 rows fetched so far", f)
		}
	}
	h.now = h.now.Add(time.Minute) // past the wait after a failure
	if r := h.ok("search_recipes", map[string]any{"query": "lentil"}); r.TotalMatches != 1 {
		t.Fatalf("after the sync completed: %d matches", r.TotalMatches)
	}
}
