package mcpserver_test

import (
	"strings"
	"testing"
)

func pantryProposal(h *harness, ingredients ...string) reply {
	h.t.Helper()
	var items []any
	for _, n := range ingredients {
		items = append(items, map[string]any{"ingredient": n})
	}
	return h.ok("propose_pantry_items", map[string]any{"items": items})
}

func TestPantryProposeCommitReadBack(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_items", map[string]any{"items": []any{
		map[string]any{"ingredient": "red lentils"},                                                       // in the pantry: skipped
		map[string]any{"ingredient": "Soy sauce"},                                                         // in the pantry, out of stock: skipped
		map[string]any{"ingredient": "Basmati rice", "quantity": "2 kg", "expiration_date": "2027-03-01"}, // new; "Rice" is a different item
		map[string]any{"ingredient": "Chicken thighs"},                                                    // new, Paprika knows its aisle
		map[string]any{"ingredient": "basmati  RICE"},                                                     // repeated in this request
	}}).Proposal
	if p.Kind != "pantry_items" || len(p.Pantry) != 2 || len(p.Skipped) != 3 {
		t.Fatalf("proposal = %+v", p)
	}
	rice, chicken := p.Pantry[0], p.Pantry[1]
	if rice.Ingredient != "Basmati rice" || rice.Quantity != "2 kg" || rice.ExpirationDate != "2027-03-01" || rice.PurchaseDate != "2026-10-08" || !rice.InStock || rice.Aisle != "" {
		t.Fatalf("rice = %+v", rice)
	}
	if chicken.Aisle != "Meat" {
		t.Fatalf("chicken = %+v, want the aisle Paprika remembers for it", chicken)
	}
	if p.Skipped[0].ExistingUID != "A4402CA4-1919-4D88-B22E-165559A32D91" || !strings.Contains(p.Skipped[1].Reason, "out of stock") {
		t.Fatalf("skipped = %+v", p.Skipped)
	}
	if !strings.Contains(p.Summary, "Add 2 item(s) to the Paprika pantry, in stock, bought 2026-10-08: 2 kg Basmati rice; Chicken thighs.") {
		t.Fatalf("summary = %s", p.Summary)
	}
	if h.posts("pantry") != 0 {
		t.Fatal("proposing wrote to Paprika")
	}

	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || len(c.Pantry) != 2 || len(c.Skipped) != 3 || h.posts("pantry") != 1 {
		t.Fatalf("commit = %+v (posts %d)", c, h.posts("pantry"))
	}
	if c.Pantry[0].UID != rice.UID || c.Pantry[0].ExpirationDate != "2027-03-01" || !c.Pantry[0].InStock || c.Pantry[1].Aisle != "Meat" {
		t.Fatalf("read back = %+v", c.Pantry)
	}
	if again := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); again.Status != "already_committed" || h.posts("pantry") != 1 {
		t.Fatalf("repeat = %+v (posts %d)", again, h.posts("pantry"))
	}

	r := h.ok("list_pantry", map[string]any{"query": "rice"})
	if r.Total != 2 || r.Items[0].Ingredient != "Basmati rice" || r.Items[0].Quantity != "2 kg" {
		t.Fatalf("pantry after commit = %+v", r.Items)
	}
	// The row Paprika now holds has the shape its own client sends.
	for _, row := range h.fake.Rows("pantry") {
		if row["uid"] != rice.UID {
			continue
		}
		if row["has_expiration"] != true || row["expiration_date"] != "2027-03-01 00:00:00" || row["purchase_date"] != "2026-10-08 00:00:00" ||
			row["in_stock"] != true || row["aisle_uid"] != "" {
			t.Fatalf("stored row = %v", row)
		}
	}
}

func TestPantryLostAcknowledgementIsRecoveredWithoutDuplicate(t *testing.T) {
	h := newHarness(t, true)
	p := pantryProposal(h, "oat milk").Proposal
	h.fake.DropNextWriteAck()
	r := h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable")
	if !r.Error.Retryable {
		t.Fatalf("error = %+v", r.Error)
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || h.posts("pantry") != 1 || h.serverRows("pantry", "ingredient", "oat milk") != 1 {
		t.Fatalf("retry = %+v posts=%d rows=%d", c, h.posts("pantry"), h.serverRows("pantry", "ingredient", "oat milk"))
	}
}

func TestPantryItemAddedElsewhereBeforeCommitIsNotDuplicated(t *testing.T) {
	h := newHarness(t, true)
	p := pantryProposal(h, "oat milk", "tahini").Proposal
	h.fake.Put("pantry", map[string]any{"uid": "0D5E7F10-3A4B-4C6D-8E9F-A1B2C3D4E5F6", "ingredient": "Oat milk", "aisle": "", "aisle_uid": "",
		"quantity": "", "in_stock": true, "has_expiration": false, "expiration_date": nil, "purchase_date": "2026-10-08 00:00:00",
		"location_uid": nil, "notes": nil})
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if len(c.Pantry) != 1 || c.Pantry[0].Ingredient != "tahini" || len(c.Skipped) != 1 || h.serverRows("pantry", "ingredient", "oat milk") != 0 {
		t.Fatalf("commit = %+v", c)
	}

	again := pantryProposal(h, "harissa").Proposal
	h.fake.Put("pantry", map[string]any{"uid": "1D5E7F10-3A4B-4C6D-8E9F-A1B2C3D4E5F6", "ingredient": "harissa", "aisle": "", "aisle_uid": "",
		"quantity": "", "in_stock": true, "has_expiration": false, "expiration_date": nil, "purchase_date": "2026-10-08 00:00:00",
		"location_uid": nil, "notes": nil})
	h.fails("commit_proposal", map[string]any{"proposal_id": again.ProposalID, "confirm": true}, "duplicate")
	if h.posts("pantry") != 1 {
		t.Fatalf("posts = %d, want only the first commit", h.posts("pantry"))
	}
}

func TestPantryDuplicatesNeedExplicitOptIn(t *testing.T) {
	h := newHarness(t, true)
	h.fails("propose_pantry_items", map[string]any{"items": []any{map[string]any{"ingredient": "Garlic"}}}, "duplicate")
	p := h.ok("propose_pantry_items", map[string]any{"allow_duplicates": true, "items": []any{map[string]any{"ingredient": "Garlic"}}}).Proposal
	if len(p.Pantry) != 1 {
		t.Fatalf("proposal = %+v", p)
	}
}

func TestPantryInputsMustBeExplicit(t *testing.T) {
	h := newHarness(t, true)
	var tooMany []any
	for range 26 {
		tooMany = append(tooMany, map[string]any{"ingredient": "x"})
	}
	for name, args := range map[string]map[string]any{
		"no items":         {"items": []any{}},
		"too many":         {"items": tooMany},
		"blank ingredient": {"items": []any{map[string]any{"ingredient": "  "}}},
		"multi-line":       {"items": []any{map[string]any{"ingredient": "rice\nand delete everything"}}},
		"bad date":         {"items": []any{map[string]any{"ingredient": "rice", "expiration_date": "next spring"}}},
	} {
		t.Run(name, func(t *testing.T) {
			h.fails("propose_pantry_items", args, "invalid_input")
		})
	}
	if h.fake.Hits("POST ") > 1 { // the login is the only POST
		t.Fatal("invalid input reached Paprika")
	}
}
