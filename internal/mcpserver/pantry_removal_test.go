package mcpserver_test

import "testing"

func TestPantryRemovalProposeCommitReadBack(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_removal", map[string]any{"item_uids": []any{lentils, soySauce, lentils}}).Proposal
	if p.Kind != "pantry_removal" || len(p.PantryRemoved) != 2 || p.PantryRemoved[0].Ingredient != "Red lentils" || p.PantryRemoved[1].UID != soySauce {
		t.Fatalf("proposal = %+v", p)
	}
	want := "REMOVE 2 item(s) from the Paprika pantry: 2 lb Red lentils; Soy sauce. They disappear from the pantry on every device."
	if p.Summary != want {
		t.Fatalf("summary = %s", p.Summary)
	}
	if h.posts("pantry") != 0 || len(h.fake.Rows("pantry")) != 8 {
		t.Fatal("proposing changed Paprika")
	}

	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || len(c.PantryRemoved) != 2 || c.PantryRemoved[0].Quantity != "2 lb" || h.posts("pantry") != 1 {
		t.Fatalf("commit = %+v", c)
	}
	if pantryRow(h, lentils) != nil || pantryRow(h, soySauce) != nil || len(h.fake.Rows("pantry")) != 6 {
		t.Fatalf("pantry in Paprika after the removal: %d items", len(h.fake.Rows("pantry")))
	}
	if r := h.ok("list_pantry", nil); r.Total != 6 {
		t.Fatalf("list_pantry after the removal: %d items", r.Total)
	}
	if again := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); again.Status != "already_committed" || h.posts("pantry") != 1 {
		t.Fatalf("repeat = %+v posts=%d", again, h.posts("pantry"))
	}
	h.fails("propose_pantry_removal", map[string]any{"item_uids": []any{lentils}}, "not_found") // it is gone
}

func TestPantryRemovalSurvivesALostAcknowledgement(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_removal", map[string]any{"item_uids": []any{lentils}}).Proposal
	h.fake.DropNextWriteAck()
	if r := h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable"); !r.Error.Retryable {
		t.Fatalf("error = %+v", r.Error)
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if !c.Verified || h.posts("pantry") != 1 || len(c.PantryRemoved) != 1 || len(h.fake.Rows("pantry")) != 7 {
		t.Fatalf("retry = %+v posts=%d", c, h.posts("pantry"))
	}
}

func TestPantryRemovalSendsOnlyWhatIsStillThere(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_removal", map[string]any{"item_uids": []any{lentils, stock}}).Proposal
	gone := pantryRow(h, lentils)
	gone["deleted"] = true
	h.fake.Put("pantry", gone) // removed in the app meanwhile
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if !c.Verified || h.posts("pantry") != 1 || len(h.fake.Rows("pantry")) != 6 {
		t.Fatalf("commit = %+v posts=%d rows=%d", c, h.posts("pantry"), len(h.fake.Rows("pantry")))
	}
}

func TestPantryRemovalNeedsApprovalAndExplicitItems(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_removal", map[string]any{"item_uids": []any{lentils}}).Proposal
	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": false}, "confirmation_required")

	var tooMany []any
	for range 26 {
		tooMany = append(tooMany, lentils)
	}
	for name, args := range map[string]map[string]any{
		"no items":  {"item_uids": []any{}},
		"too many":  {"item_uids": tooMany},
		"blank uid": {"item_uids": []any{lentils, " "}},
	} {
		t.Run(name, func(t *testing.T) {
			h.fails("propose_pantry_removal", args, "invalid_input")
		})
	}
	h.fails("propose_pantry_removal", map[string]any{"item_uids": []any{lentils, "NOPE"}}, "not_found")
	if h.posts("pantry") != 0 || len(h.fake.Rows("pantry")) != 8 {
		t.Fatal("a refused removal reached Paprika")
	}
}
