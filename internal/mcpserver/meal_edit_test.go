package mcpserver_test

import (
	"strings"
	"testing"
)

const (
	soupMeal   = "BF0FF316-1A3F-4AEF-BCD1-87FC6B21586C" // Red Lentil Soup, Dinner, Monday 2026-10-05
	stirFryDay = "2026-10-07"                           // Weeknight Chicken Stir-Fry is Dinner that day
	stirFryUID = "3B9E1D42-6A7C-4F85-9D20-1C8E5A4B7F63"
	lunch      = "74B7DE10D8791D7B501CB5DC41365994F2CC80227B7CE5CB2548E24AF26DC939"
)

func mealRow(h *harness, uid string) map[string]any {
	for _, r := range h.fake.Rows("meals") {
		if r["uid"] == uid {
			return r
		}
	}
	return nil
}

func TestMealEditProposeCommitReadBack(t *testing.T) {
	h := newHarness(t, true)
	// A row carrying things the adapter does not model: they must survive the edit.
	scaled := mealRow(h, soupMeal)
	scaled["scale"], scaled["field_from_a_newer_paprika"] = "2x", "keep me"
	h.fake.Put("meals", scaled)

	p := h.ok("propose_meal_plan_edit", map[string]any{"meal_uid": soupMeal, "date": "2026-10-09", "meal_type_uid": lunch, "recipe_uid": roastUID}).Proposal
	if p.Kind != "meal_plan_edit" || p.MealBefore == nil || p.Meal == nil {
		t.Fatalf("proposal = %+v", p)
	}
	if b := p.MealBefore; b.Name != "Red Lentil Soup" || b.Date != "2026-10-05" || b.MealType != "Dinner" {
		t.Fatalf("before = %+v", b)
	}
	if a := p.Meal; a.UID != soupMeal || a.Name != "Lemon Garlic Roast Chicken" || a.RecipeUID != roastUID || a.Date != "2026-10-09" || a.MealType != "Lunch" {
		t.Fatalf("after = %+v", a)
	}
	want := `Change the Paprika meal plan entry "Red Lentil Soup" (Dinner on 2026-10-05 (Monday)): move it to 2026-10-09 (Friday); make it Lunch; ` +
		`plan "Lemon Garlic Roast Chicken" instead. This overwrites the entry.`
	if p.Summary != want {
		t.Fatalf("summary = %s", p.Summary)
	}
	if h.posts("meals") != 0 {
		t.Fatal("proposing wrote to Paprika")
	}

	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || c.Meal == nil || c.Meal.Date != "2026-10-09" || c.Meal.MealType != "Lunch" || c.MealBefore.Date != "2026-10-05" || h.posts("meals") != 1 {
		t.Fatalf("commit = %+v meal %+v", c, c.Meal)
	}
	row := mealRow(h, soupMeal)
	if row["date"] != "2026-10-09 00:00:00" || row["type_uid"] != lunch || row["type"] != float64(1) || row["recipe_uid"] != roastUID || row["name"] != "Lemon Garlic Roast Chicken" {
		t.Fatalf("row in Paprika = %v", row)
	}
	if row["scale"] != "2x" || row["field_from_a_newer_paprika"] != "keep me" || row["is_ingredient"] != false || row["order_flag"] != float64(0) {
		t.Fatalf("a field the edit did not name was lost: %v", row)
	}
	if len(h.fake.Rows("meals")) != 5 {
		t.Fatalf("the edit created or removed a row: %d meals", len(h.fake.Rows("meals")))
	}
	plan := h.ok("get_meal_plan", nil).Meals
	if len(plan) != 4 || plan[0].Date == "2026-10-05" {
		t.Fatalf("plan after the edit = %+v", plan)
	}
	if again := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); again.Status != "already_committed" || h.posts("meals") != 1 {
		t.Fatalf("repeat = %+v posts=%d", again, h.posts("meals"))
	}
}

func TestMealEditToFreeTextSurvivesALostAcknowledgement(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_meal_plan_edit", map[string]any{"meal_uid": soupMeal, "name": "Takeaway"}).Proposal
	if !strings.Contains(p.Summary, `plan "Takeaway" instead`) || p.Meal.RecipeUID != "" || p.Meal.Date != "2026-10-05" {
		t.Fatalf("proposal = %+v", p)
	}
	h.fake.DropNextWriteAck()
	if r := h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable"); !r.Error.Retryable {
		t.Fatalf("error = %+v", r.Error)
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	row := mealRow(h, soupMeal)
	if !c.Verified || h.posts("meals") != 1 || row["name"] != "Takeaway" || row["recipe_uid"] != nil {
		t.Fatalf("retry = %+v posts=%d row=%v", c, h.posts("meals"), row)
	}
}

func TestMealEditOfAnEntryRemovedMeanwhileWritesNothing(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_meal_plan_edit", map[string]any{"meal_uid": soupMeal, "date": "2026-10-09"}).Proposal
	gone := mealRow(h, soupMeal)
	gone["deleted"] = true
	h.fake.Put("meals", gone) // removed in the app
	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "not_found")
	if h.posts("meals") != 0 {
		t.Fatal("the edit brought a removed entry back")
	}
}

func TestMealEditDoesNotDoublePlanByAccident(t *testing.T) {
	h := newHarness(t, true)
	args := map[string]any{"meal_uid": soupMeal, "date": stirFryDay, "recipe_uid": stirFryUID}
	h.fails("propose_meal_plan_edit", args, "duplicate") // the stir-fry is already dinner that day
	args["allow_duplicate"] = true
	p := h.ok("propose_meal_plan_edit", args).Proposal
	if c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); !c.Verified {
		t.Fatalf("commit = %+v", c)
	}
}

func TestMealEditInputsMustBeExplicit(t *testing.T) {
	h := newHarness(t, true)
	for name, args := range map[string]map[string]any{
		"nothing to change": {"meal_uid": soupMeal},
		"blank uid":         {"meal_uid": " ", "date": "2026-10-09"},
		"recipe and text":   {"meal_uid": soupMeal, "recipe_uid": roastUID, "name": "Leftovers"},
		"bad date":          {"meal_uid": soupMeal, "date": "next friday"},
		"wrong year":        {"meal_uid": soupMeal, "date": "2029-10-09"},
		"recipe in trash":   {"meal_uid": soupMeal, "recipe_uid": trashedUID},
	} {
		t.Run(name, func(t *testing.T) {
			h.fails("propose_meal_plan_edit", args, "invalid_input")
		})
	}
	h.fails("propose_meal_plan_edit", map[string]any{"meal_uid": "NOPE", "date": "2026-10-09"}, "not_found")
	h.fails("propose_meal_plan_edit", map[string]any{"meal_uid": soupMeal, "meal_type_uid": "NOPE"}, "not_found")
	h.fails("propose_meal_plan_edit", map[string]any{"meal_uid": soupMeal, "recipe_uid": "NOPE"}, "not_found")
	h.fails("propose_meal_plan_edit", map[string]any{"meal_uid": soupMeal, "date": "2026-10-05", "meal_type_uid": dinner}, "duplicate") // already so
	if h.posts("meals") != 0 {
		t.Fatal("a refused edit reached Paprika")
	}
}
