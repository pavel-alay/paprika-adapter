package mcpserver_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"paprika-adapter/internal/mcpserver"
)

func groceryProposal(h *harness, names ...string) reply {
	h.t.Helper()
	var items []any
	for _, n := range names {
		items = append(items, map[string]any{"name": n})
	}
	return h.ok("propose_grocery_items", map[string]any{"list_uid": defaultList, "items": items})
}

func TestGroceryProposeCommitReadBack(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_grocery_items", map[string]any{"list_uid": defaultList, "items": []any{
		map[string]any{"name": "milk"},                                              // on the list, unpurchased: skipped
		map[string]any{"name": "Eggs", "quantity": "1 dozen"},                       // on the list but purchased: added
		map[string]any{"name": "lemons", "quantity": "3", "instruction": "unwaxed"}, // on the list as "2 lemons": skipped
		map[string]any{"name": "Chicken thighs", "quantity": "1 lb"},                // new, Paprika knows its aisle
		map[string]any{"name": "sparkling water"},                                   // new, no aisle known
		map[string]any{"name": "Sparkling  Water"},                                  // repeated in this request
	}}).Proposal
	if len(p.Items) != 3 || len(p.Skipped) != 3 || p.Kind != "grocery_items" || p.ExpiresAt != "2026-10-08T12:15:00Z" {
		t.Fatalf("proposal = %+v", p)
	}
	if p.Items[0].Name != "1 dozen Eggs" || p.Items[1].Aisle != "Meat" || p.Items[2].Aisle != "" {
		t.Fatalf("proposed items = %+v", p.Items)
	}
	if p.Skipped[0].ExistingUID != "0A91A12D-74D3-4987-BBBE-C36D3FABF505" || p.Skipped[1].Name != "lemons" {
		t.Fatalf("skipped = %+v", p.Skipped)
	}
	if !strings.Contains(p.Summary, `Add 3 item(s) to the Paprika grocery list "My Grocery List": 1 dozen Eggs; 1 lb Chicken thighs; sparkling water.`) {
		t.Fatalf("summary = %s", p.Summary)
	}
	if h.posts("groceries") != 0 {
		t.Fatal("proposing wrote to Paprika")
	}

	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": false}, "confirmation_required")
	if h.posts("groceries") != 0 {
		t.Fatal("unconfirmed commit wrote to Paprika")
	}

	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || len(c.Items) != 3 || len(c.Skipped) != 3 || h.posts("groceries") != 1 {
		t.Fatalf("commit = %+v (posts %d)", c, h.posts("groceries"))
	}
	for i, it := range c.Items {
		if it.UID != p.Items[i].UID || it.Name != p.Items[i].Name || it.Purchased {
			t.Errorf("read-back item %d = %+v, proposed %+v", i, it, p.Items[i])
		}
	}

	r := h.ok("list_grocery_items", map[string]any{"list_uid": defaultList})
	if r.Total != 6 || r.Items[5].Name != "sparkling water" || r.Items[4].Aisle != "Meat" {
		t.Fatalf("list after commit = %+v", r.Items)
	}
	if h.ok("list_grocery_items", map[string]any{"list_uid": costcoList}).Total != 1 {
		t.Fatal("another list was touched")
	}
}

func TestRepeatedCommitWritesOnce(t *testing.T) {
	h := newHarness(t, true)
	p := groceryProposal(h, "oat milk").Proposal
	first := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	requests := h.fake.Hits("")
	for range 3 {
		again := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
		if again.Status != "already_committed" || !again.Verified || again.Items[0].UID != first.Items[0].UID {
			t.Fatalf("repeat = %+v", again)
		}
	}
	if h.fake.Hits("") != requests || h.posts("groceries") != 1 || h.serverRows("groceries", "ingredient", "oat milk") != 1 {
		t.Fatalf("repeats touched Paprika: posts=%d rows=%d", h.posts("groceries"), h.serverRows("groceries", "ingredient", "oat milk"))
	}
}

func TestLostAcknowledgementIsRecoveredWithoutDuplicate(t *testing.T) {
	h := newHarness(t, true)
	p := groceryProposal(h, "oat milk").Proposal
	h.fake.DropNextWriteAck() // Paprika stores the item, the response never arrives
	r := h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable")
	if !r.Error.Retryable || !strings.Contains(r.Error.Message, "commit_proposal again") {
		t.Fatalf("error = %+v", r.Error)
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || h.posts("groceries") != 1 || h.serverRows("groceries", "ingredient", "oat milk") != 1 {
		t.Fatalf("retry = %+v posts=%d rows=%d", c, h.posts("groceries"), h.serverRows("groceries", "ingredient", "oat milk"))
	}
}

func TestFailedWriteCanBeRetried(t *testing.T) {
	h := newHarness(t, true)
	p := groceryProposal(h, "oat milk").Proposal
	h.fake.FailNext("POST /api/v2/sync/groceries/", 503) // Paprika stores nothing
	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable")
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if !c.Verified || h.posts("groceries") != 2 || h.serverRows("groceries", "ingredient", "oat milk") != 1 {
		t.Fatalf("retry = %+v posts=%d", c, h.posts("groceries"))
	}
}

func TestSameItemAddedElsewhereBeforeCommitIsNotDuplicated(t *testing.T) {
	h := newHarness(t, true)
	p := groceryProposal(h, "oat milk", "tofu").Proposal
	h.fake.Put("groceries", map[string]any{"uid": "FROM-THE-PHONE", "list_uid": defaultList, "name": "Oat Milk", "ingredient": "oat milk", "purchased": false})
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if len(c.Items) != 1 || c.Items[0].Name != "tofu" || len(c.Skipped) != 1 || c.Skipped[0].ExistingUID != "FROM-THE-PHONE" {
		t.Fatalf("commit = items %+v skipped %+v", c.Items, c.Skipped)
	}
	if h.serverRows("groceries", "ingredient", "oat milk") != 1 {
		t.Fatal("duplicate oat milk on the server")
	}

	only := groceryProposal(h, "rice noodles").Proposal
	h.fake.Put("groceries", map[string]any{"uid": "FROM-THE-PHONE-2", "list_uid": defaultList, "name": "rice noodles", "ingredient": "rice noodles", "purchased": false})
	posts := h.posts("groceries")
	h.fails("commit_proposal", map[string]any{"proposal_id": only.ProposalID, "confirm": true}, "duplicate")
	if h.posts("groceries") != posts {
		t.Fatal("a fully duplicated proposal still wrote")
	}
}

func TestSecondProposalForSameItemsCannotDoubleAdd(t *testing.T) {
	h := newHarness(t, true)
	a, b := groceryProposal(h, "oat milk").Proposal, groceryProposal(h, "oat milk").Proposal
	h.ok("commit_proposal", map[string]any{"proposal_id": a.ProposalID, "confirm": true})
	h.fails("commit_proposal", map[string]any{"proposal_id": b.ProposalID, "confirm": true}, "duplicate")
	if h.serverRows("groceries", "ingredient", "oat milk") != 1 {
		t.Fatal("two proposals produced two items")
	}
}

func TestDuplicatesNeedExplicitOptIn(t *testing.T) {
	h := newHarness(t, true)
	h.fails("propose_grocery_items", map[string]any{"list_uid": defaultList, "items": []any{map[string]any{"name": "Milk"}}}, "duplicate")
	p := h.ok("propose_grocery_items", map[string]any{"list_uid": defaultList, "allow_duplicates": true,
		"items": []any{map[string]any{"name": "Milk"}}}).Proposal
	h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if h.serverRows("groceries", "ingredient", "Milk")+h.serverRows("groceries", "ingredient", "milk") != 2 {
		t.Fatal("explicitly allowed duplicate was not written")
	}
}

func TestProposalExpires(t *testing.T) {
	h := newHarness(t, true)
	p := groceryProposal(h, "oat milk").Proposal
	h.now = h.now.Add(16 * time.Minute)
	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "proposal_expired")
	h.fails("commit_proposal", map[string]any{"proposal_id": "NO-SUCH-PROPOSAL", "confirm": true}, "not_found")
	if h.posts("groceries") != 0 {
		t.Fatal("expired or unknown proposal wrote to Paprika")
	}
}

func TestGroceryInputsMustBeExplicit(t *testing.T) {
	h := newHarness(t, true)
	one := []any{map[string]any{"name": "tea"}}
	many := make([]any, 26)
	for i := range many {
		many[i] = map[string]any{"name": "thing"}
	}
	for name, args := range map[string]map[string]any{
		"no list":        {"list_uid": " ", "items": one},
		"no items":       {"list_uid": defaultList, "items": []any{}},
		"too many items": {"list_uid": defaultList, "items": many},
		"blank name":     {"list_uid": defaultList, "items": []any{map[string]any{"name": "  "}}},
		"multi-line":     {"list_uid": defaultList, "items": []any{map[string]any{"name": "tea\nmilk"}}},
		"very long":      {"list_uid": defaultList, "items": []any{map[string]any{"name": strings.Repeat("x", 201)}}},
	} {
		t.Run(name, func(t *testing.T) { h.fails("propose_grocery_items", args, "invalid_input") })
	}
	h.fails("propose_grocery_items", map[string]any{"list_uid": "NOPE", "items": one}, "not_found")
}

func TestNoWriteIsPreparedWhilePaprikaIsUnreachable(t *testing.T) {
	h := newHarness(t, true)
	h.ok("list_grocery_lists", nil)
	h.fake.SetDown(true)
	h.now = h.now.Add(time.Hour)
	r := h.fails("propose_grocery_items", map[string]any{"list_uid": defaultList, "items": []any{map[string]any{"name": "tea"}}}, "upstream_unavailable")
	if r.Proposal != nil {
		t.Fatal("a proposal was created from stale data")
	}
}

func TestMealProposeCommitReadBack(t *testing.T) {
	h := newHarness(t, true)
	args := map[string]any{"date": "2026-10-09", "meal_type_uid": dinner, "recipe_uid": roastUID}
	p := h.ok("propose_meal_plan_entry", args).Proposal
	if p.Summary != `Add "Lemon Garlic Roast Chicken" as Dinner on 2026-10-09 (Friday) to the Paprika meal plan.` || p.Meal.RecipeUID != roastUID {
		t.Fatalf("proposal = %+v", p)
	}
	if h.posts("meals") != 0 {
		t.Fatal("proposing wrote to Paprika")
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || c.Meal.UID != p.Meal.UID || c.Meal.MealType != "Dinner" || c.Meal.Date != "2026-10-09" {
		t.Fatalf("commit = %+v meal %+v", c, c.Meal)
	}
	stored := h.fake.Rows("meals")[5]
	if stored["type"] != float64(2) || stored["type_uid"] != dinner || stored["date"] != "2026-10-09 00:00:00" || stored["recipe_uid"] != roastUID {
		t.Fatalf("row stored by Paprika = %+v", stored)
	}

	plan := h.ok("get_meal_plan", nil).Meals
	if len(plan) != 5 || plan[3].UID != p.Meal.UID {
		t.Fatalf("plan after commit = %+v", plan)
	}

	h.fails("propose_meal_plan_entry", args, "duplicate")
	if again := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); again.Status != "already_committed" || h.posts("meals") != 1 {
		t.Fatalf("repeat = %+v posts=%d", again, h.posts("meals"))
	}
}

func TestFreeTextMealAndLostAcknowledgement(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_meal_plan_entry", map[string]any{"date": "2026-10-11", "meal_type_uid": dinner, "name": "Takeaway"}).Proposal
	h.fake.DropNextWriteAck()
	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable")
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if !c.Verified || c.Meal.Name != "Takeaway" || c.Meal.RecipeUID != "" || h.posts("meals") != 1 || h.serverRows("meals", "name", "Takeaway") != 1 {
		t.Fatalf("commit = %+v posts=%d", c.Meal, h.posts("meals"))
	}
}

func TestMealInputsMustBeExplicit(t *testing.T) {
	h := newHarness(t, true)
	for name, tc := range map[string]struct {
		args map[string]any
		code string
	}{
		"bad date":          {map[string]any{"date": "friday", "meal_type_uid": dinner, "name": "x"}, "invalid_input"},
		"wrong year":        {map[string]any{"date": "2062-10-09", "meal_type_uid": dinner, "name": "x"}, "invalid_input"},
		"recipe and name":   {map[string]any{"date": "2026-10-09", "meal_type_uid": dinner, "name": "x", "recipe_uid": roastUID}, "invalid_input"},
		"neither":           {map[string]any{"date": "2026-10-09", "meal_type_uid": dinner}, "invalid_input"},
		"blank meal type":   {map[string]any{"date": "2026-10-09", "meal_type_uid": "", "name": "x"}, "invalid_input"},
		"unknown meal type": {map[string]any{"date": "2026-10-09", "meal_type_uid": "NOPE", "name": "x"}, "not_found"},
		"unknown recipe":    {map[string]any{"date": "2026-10-09", "meal_type_uid": dinner, "recipe_uid": "NOPE"}, "not_found"},
		"trashed recipe":    {map[string]any{"date": "2026-10-09", "meal_type_uid": dinner, "recipe_uid": trashedUID}, "invalid_input"},
		"already planned":   {map[string]any{"date": "2026-10-05", "meal_type_uid": dinner, "recipe_uid": soupUID}, "duplicate"},
	} {
		t.Run(name, func(t *testing.T) { h.fails("propose_meal_plan_entry", tc.args, tc.code) })
	}
	if h.posts("meals") != 0 {
		t.Fatal("a rejected proposal wrote to Paprika")
	}
}

// The approval prompt travels differently before and after MCP 2026-07-28, so
// both paths are exercised.
var protocols = []string{"2025-06-18", "2025-11-25", ""}

func TestUserApprovesThroughTheClient(t *testing.T) {
	for _, protocol := range protocols {
		t.Run("protocol "+protocol, func(t *testing.T) {
			h := newHarness(t, true, func(h *harness) { h.approval, h.protocol = "accept", protocol })
			p := groceryProposal(h, "oat milk").Proposal
			c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
			if !c.Verified || len(h.prompts) != 1 || !strings.Contains(h.prompts[0], p.Summary) || h.posts("groceries") != 1 {
				t.Fatalf("commit = %+v prompts = %q", c, h.prompts)
			}
			h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
			if len(h.prompts) != 1 {
				t.Fatal("an already committed proposal prompted the user again")
			}
			h.fails("commit_proposal", map[string]any{"proposal_id": groceryProposal(h, "tofu").Proposal.ProposalID, "confirm": false}, "confirmation_required")
			if len(h.prompts) != 1 {
				t.Fatal("the user was prompted for a commit the agent had not confirmed")
			}
		})
	}
}

func TestUserDeclinesAndNothingIsWritten(t *testing.T) {
	for _, protocol := range protocols {
		for _, answer := range []string{"decline", "cancel"} {
			t.Run(answer+" protocol "+protocol, func(t *testing.T) {
				h := newHarness(t, true, func(h *harness) { h.approval, h.protocol = answer, protocol })
				p := h.ok("propose_meal_plan_entry", map[string]any{"date": "2026-10-09", "meal_type_uid": dinner, "name": "Takeaway"}).Proposal
				h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "approval_declined")
				if h.posts("meals") != 0 || len(h.prompts) != 1 {
					t.Fatalf("posts=%d prompts=%d", h.posts("meals"), len(h.prompts))
				}
			})
		}
	}
}

func TestHumanApprovalCanBeMandatory(t *testing.T) {
	h := newHarness(t, true, func(h *harness) { h.mode = mcpserver.ApprovalHuman }) // client cannot prompt
	p := groceryProposal(h, "oat milk").Proposal
	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "approval_unavailable")
	if h.posts("groceries") != 0 {
		t.Fatal("wrote without the mandatory approval")
	}
}

func TestApprovalNoneWritesWithoutAskingAnyone(t *testing.T) {
	h := newHarness(t, true, func(h *harness) { h.approval, h.mode = "decline", mcpserver.ApprovalNone }) // the client could prompt
	p := groceryProposal(h, "oat milk").Proposal
	if r := h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": false}, "confirmation_required"); strings.Contains(r.Error.Message, "approved") {
		t.Fatalf("error = %+v", r.Error)
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if !c.Verified || len(h.prompts) != 0 || h.posts("groceries") != 1 {
		t.Fatalf("commit = %+v prompts = %q posts = %d", c, h.prompts, h.posts("groceries"))
	}
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	told := h.session.InitializeResult().Instructions
	for _, tool := range res.Tools {
		told += tool.Description
	}
	if strings.Contains(told, "approve") || !strings.Contains(told, "asks for no approval") {
		t.Fatalf("the agent is still told to seek approval: %s", told)
	}
}
