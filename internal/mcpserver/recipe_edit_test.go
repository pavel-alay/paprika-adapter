package mcpserver_test

import (
	"strings"
	"testing"
)

const newDirections = "Heat the oven to 220C.\nRoast for 1 hour 20 minutes, then rest."

func recipePosts(h *harness, uid string) int { return h.posts("recipe/" + uid) }

func TestRecipeEditProposeCommitReadBack(t *testing.T) {
	h := newHarness(t, true)
	before := h.fake.Recipe(roastUID)
	p := h.ok("propose_recipe_edit", map[string]any{"uid": roastUID, "rating": 3, "append_note": "Next time: less salt.",
		"description": "Sunday roast.", "directions": newDirections}).Proposal
	if p.Kind != "recipe_edit" || p.Recipe == nil || p.Recipe.Recipe.Name != "Lemon Garlic Roast Chicken" || len(p.Recipe.Changes) != 4 {
		t.Fatalf("proposal = %+v", p)
	}
	if c := p.Recipe.Changes[0]; c.Field != "rating" || c.From != "5" || c.To != "3" {
		t.Fatalf("rating change = %+v", c)
	}
	if c := p.Recipe.Changes[3]; c.Field != "directions" || !strings.HasPrefix(c.From, "Heat the oven to 425F.") || c.To != newDirections {
		t.Fatalf("directions change = %+v", c)
	}
	for _, want := range []string{`"Lemon Garlic Roast Chicken"`, "rating 5 of 5 -> 3 of 5", `add to the notes: "Next time: less salt."`, "replace the directions", "overwrites"} {
		if !strings.Contains(p.Summary, want) {
			t.Fatalf("summary lacks %q: %s", want, p.Summary)
		}
	}
	if recipePosts(h, roastUID) != 0 {
		t.Fatal("proposing wrote to Paprika")
	}

	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || c.RecipeEdit == nil || len(c.RecipeEdit.Changes) != 4 || recipePosts(h, roastUID) != 1 {
		t.Fatalf("commit = %+v (posts %d)", c, recipePosts(h, roastUID))
	}
	after := h.fake.Recipe(roastUID)
	if after["rating"] != float64(3) || after["notes"] != "Next time: less salt." || after["description"] != "Sunday roast." || after["directions"] != newDirections {
		t.Fatalf("Paprika holds rating=%v notes=%q description=%q", after["rating"], after["notes"], after["description"])
	}
	for _, field := range []string{"name", "ingredients", "servings", "source", "created", "photo", "photo_hash", "photo_large", "on_favorites", "prep_time"} {
		if after[field] != before[field] {
			t.Errorf("%s changed from %v to %v", field, before[field], after[field])
		}
	}
	if len(after["categories"].([]any)) != len(before["categories"].([]any)) || after["hash"] == before["hash"] {
		t.Fatalf("categories or hash wrong after the edit: %v, %v", after["categories"], after["hash"])
	}

	// The cache already has the new version: no sync needed to read or find it.
	if r := h.ok("get_recipe", map[string]any{"uid": roastUID}).Recipe; r.Rating != 3 || r.Notes != "Next time: less salt." || r.Directions != newDirections {
		t.Fatalf("get_recipe after the edit: rating=%d notes=%q", r.Rating, r.Notes)
	}
	if h.ok("search_recipes", map[string]any{"query": "less salt"}).TotalMatches != 1 {
		t.Fatal("the new note is not searchable")
	}
	if again := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); again.Status != "already_committed" || recipePosts(h, roastUID) != 1 {
		t.Fatalf("repeat = %+v (posts %d)", again, recipePosts(h, roastUID))
	}
}

func TestAppendNoteKeepsExistingNotesAndIsNotAppliedTwice(t *testing.T) {
	h := newHarness(t, true)
	h.fake.UpdateRecipe(soupUID, "notes", "Good with bread.\n")
	p := h.ok("propose_recipe_edit", map[string]any{"uid": soupUID, "append_note": "Freezes well."}).Proposal
	if c := p.Recipe.Changes[0]; c.Field != "notes" || c.From != "Good with bread.\n" || c.To != "Good with bread.\n\nFreezes well." {
		t.Fatalf("change = %+v", c)
	}
	h.fake.DropNextWriteAck() // Paprika stores the recipe, the response never arrives
	if r := h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable"); !r.Error.Retryable {
		t.Fatalf("error = %+v", r.Error)
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || recipePosts(h, soupUID) != 1 || len(c.RecipeEdit.Changes) != 1 {
		t.Fatalf("retry = %+v (posts %d)", c, recipePosts(h, soupUID))
	}
	if notes := h.fake.Recipe(soupUID)["notes"]; notes != "Good with bread.\n\nFreezes well." {
		t.Fatalf("notes = %q", notes)
	}
}

// The user asked for plain overwrite: an edit made elsewhere to the same field
// loses, but edits to other fields survive because the commit starts from a fresh copy.
func TestRecipeEditOverwritesTheFieldAndKeepsWhatElseChanged(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_recipe_edit", map[string]any{"uid": soupUID, "notes": "Use red lentils only.", "rating": 5}).Proposal
	h.fake.UpdateRecipe(soupUID, "notes", "edited on the phone")
	h.fake.UpdateRecipe(soupUID, "servings", "8")
	h.fake.UpdateRecipe(soupUID, "field_from_a_newer_paprika", "keep me")
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	after := h.fake.Recipe(soupUID)
	if !c.Verified || after["notes"] != "Use red lentils only." || after["rating"] != float64(5) {
		t.Fatalf("after = notes %q rating %v", after["notes"], after["rating"])
	}
	if after["servings"] != "8" || after["field_from_a_newer_paprika"] != "keep me" {
		t.Fatalf("another field was lost: servings=%v unknown=%v", after["servings"], after["field_from_a_newer_paprika"])
	}
	if _, sent := after["photo_url"]; sent {
		t.Fatal("the signed photo link was written back into the recipe")
	}
}

func TestRecipeEditInputsMustBeExplicit(t *testing.T) {
	h := newHarness(t, true)
	for name, args := range map[string]map[string]any{
		"nothing to change": {"uid": roastUID},
		"blank uid":         {"uid": " ", "rating": 3},
		"six stars":         {"uid": roastUID, "rating": 6},
		"negative stars":    {"uid": roastUID, "rating": -1},
		"replace and add":   {"uid": roastUID, "notes": "a", "append_note": "b"},
		"empty directions":  {"uid": roastUID, "directions": " \n"},
		"too long":          {"uid": roastUID, "notes": strings.Repeat("x", 20001)},
		"in the trash":      {"uid": trashedUID, "rating": 1},
	} {
		t.Run(name, func(t *testing.T) {
			h.fails("propose_recipe_edit", args, "invalid_input")
		})
	}
	h.fails("propose_recipe_edit", map[string]any{"uid": "NOPE", "rating": 3}, "not_found")
	h.fails("propose_recipe_edit", map[string]any{"uid": roastUID, "rating": 5}, "duplicate") // already five stars
	if h.fake.Hits("POST /api/v2/sync/recipe/") != 0 {
		t.Fatal("a refused edit reached Paprika")
	}
	cleared := h.ok("propose_recipe_edit", map[string]any{"uid": soupUID, "rating": 0}).Proposal
	if !strings.Contains(cleared.Summary, "rating 4 of 5 -> unrated") {
		t.Fatalf("summary = %s", cleared.Summary)
	}
}
