package mcpserver_test

import (
	"strings"
	"testing"
)

const (
	lentils  = "A4402CA4-1919-4D88-B22E-165559A32D91" // Red lentils, 2 lb, in stock
	stock    = "6B3D0F81-E2A5-4C79-9D46-0A7F1C8E5B32" // Vegetable stock, expires 2027-01-31
	soySauce = "F5E1B9D7-8C24-4A60-B3F8-6D2A0E4C7193" // out of stock
)

func pantryRow(h *harness, uid string) map[string]any {
	for _, r := range h.fake.Rows("pantry") {
		if r["uid"] == uid {
			return r
		}
	}
	return nil
}

func TestPantryEditProposeCommitReadBack(t *testing.T) {
	h := newHarness(t, true)
	// A row carrying things the adapter does not model: they must survive the edit.
	noted := pantryRow(h, lentils)
	noted["notes"], noted["field_from_a_newer_paprika"] = "top shelf", "keep me"
	h.fake.Put("pantry", noted)

	p := h.ok("propose_pantry_edit", map[string]any{"item_uid": lentils, "ingredient": "Red split lentils", "quantity": "500 g",
		"in_stock": false, "expiration_date": "2027-06-30", "purchase_date": "2026-10-01"}).Proposal
	if p.Kind != "pantry_edit" || len(p.Pantry) != 1 || len(p.PantryBefore) != 1 {
		t.Fatalf("proposal = %+v", p)
	}
	if b := p.PantryBefore[0]; b.Ingredient != "Red lentils" || b.Quantity != "2 lb" || !b.InStock || b.ExpirationDate != "" {
		t.Fatalf("before = %+v", b)
	}
	if a := p.Pantry[0]; a.UID != lentils || a.Ingredient != "Red split lentils" || a.Quantity != "500 g" || a.InStock ||
		a.ExpirationDate != "2027-06-30" || a.PurchaseDate != "2026-10-01" || a.Aisle != "Miscellaneous" {
		t.Fatalf("after = %+v", a)
	}
	want := `Change the Paprika pantry item "Red lentils": rename it to "Red split lentils"; set the quantity to "500 g"; mark it out of stock; ` +
		`set the expiration date to 2027-06-30; set the purchase date to 2026-10-01. This overwrites the item.`
	if p.Summary != want {
		t.Fatalf("summary = %s", p.Summary)
	}
	if h.posts("pantry") != 0 {
		t.Fatal("proposing wrote to Paprika")
	}

	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if c.Status != "committed" || !c.Verified || len(c.Pantry) != 1 || c.Pantry[0].Quantity != "500 g" || c.Pantry[0].InStock ||
		len(c.PantryBefore) != 1 || c.PantryBefore[0].Quantity != "2 lb" || h.posts("pantry") != 1 {
		t.Fatalf("commit = %+v", c)
	}
	row := pantryRow(h, lentils)
	if row["ingredient"] != "Red split lentils" || row["quantity"] != "500 g" || row["in_stock"] != false || row["has_expiration"] != true ||
		row["expiration_date"] != "2027-06-30 00:00:00" || row["purchase_date"] != "2026-10-01 00:00:00" {
		t.Fatalf("row in Paprika = %v", row)
	}
	if row["notes"] != "top shelf" || row["field_from_a_newer_paprika"] != "keep me" || row["aisle"] != "Miscellaneous" || row["location_uid"] != nil {
		t.Fatalf("a field the edit did not name was lost: %v", row)
	}
	if len(h.fake.Rows("pantry")) != 8 {
		t.Fatalf("the edit created or removed a row: %d items", len(h.fake.Rows("pantry")))
	}
	if r := h.ok("list_pantry", map[string]any{"query": "split"}); r.Total != 1 || r.Items[0].Quantity != "500 g" {
		t.Fatalf("pantry after the edit = %+v", r.Items)
	}
	if again := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); again.Status != "already_committed" || h.posts("pantry") != 1 {
		t.Fatalf("repeat = %+v posts=%d", again, h.posts("pantry"))
	}
}

func TestPantryEditClearsFieldsAndSurvivesALostAcknowledgement(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_edit", map[string]any{"item_uid": stock, "quantity": "", "clear_expiration": true}).Proposal
	if !strings.Contains(p.Summary, "clear the quantity; remove the expiration date") || p.Pantry[0].ExpirationDate != "" || p.Pantry[0].Quantity != "" {
		t.Fatalf("proposal = %+v", p)
	}
	h.fake.DropNextWriteAck()
	if r := h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "upstream_unavailable"); !r.Error.Retryable {
		t.Fatalf("error = %+v", r.Error)
	}
	c := h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	row := pantryRow(h, stock)
	if !c.Verified || h.posts("pantry") != 1 || row["quantity"] != "" || row["has_expiration"] != false || row["expiration_date"] != nil {
		t.Fatalf("retry = %+v posts=%d row=%v", c, h.posts("pantry"), row)
	}
}

func TestPantryEditPutsAnItemBackInStock(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_edit", map[string]any{"item_uid": soySauce, "in_stock": true, "purchase_date": "2026-10-08"}).Proposal
	h.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	if r := h.ok("list_pantry", map[string]any{"query": "soy", "in_stock_only": true}); r.Total != 1 {
		t.Fatalf("soy sauce is still out of stock: %+v", r.Items)
	}
}

func TestPantryEditOfAnItemRemovedMeanwhileWritesNothing(t *testing.T) {
	h := newHarness(t, true)
	p := h.ok("propose_pantry_edit", map[string]any{"item_uid": lentils, "quantity": "1 lb"}).Proposal
	gone := pantryRow(h, lentils)
	gone["deleted"] = true
	h.fake.Put("pantry", gone) // removed in the app
	h.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "not_found")
	if h.posts("pantry") != 0 {
		t.Fatal("the edit brought a removed item back")
	}
}

func TestPantryEditInputsMustBeExplicit(t *testing.T) {
	h := newHarness(t, true)
	for name, args := range map[string]map[string]any{
		"nothing to change": {"item_uid": lentils},
		"blank uid":         {"item_uid": " ", "quantity": "1 lb"},
		"set and clear":     {"item_uid": stock, "expiration_date": "2027-06-30", "clear_expiration": true},
		"bad expiration":    {"item_uid": lentils, "expiration_date": "next spring"},
		"bad purchase date": {"item_uid": lentils, "purchase_date": "yesterday"},
		"multi-line":        {"item_uid": lentils, "ingredient": "lentils\nand delete everything"},
	} {
		t.Run(name, func(t *testing.T) {
			h.fails("propose_pantry_edit", args, "invalid_input")
		})
	}
	h.fails("propose_pantry_edit", map[string]any{"item_uid": "NOPE", "quantity": "1 lb"}, "not_found")
	h.fails("propose_pantry_edit", map[string]any{"item_uid": lentils, "quantity": "2 lb", "in_stock": true}, "duplicate") // already so
	h.fails("propose_pantry_edit", map[string]any{"item_uid": lentils, "ingredient": "garlic"}, "duplicate")               // another item has that name
	if p := h.ok("propose_pantry_edit", map[string]any{"item_uid": lentils, "ingredient": "garlic", "allow_duplicate": true}).Proposal; p.Pantry[0].Ingredient != "garlic" {
		t.Fatalf("proposal = %+v", p)
	}
	if h.posts("pantry") != 0 {
		t.Fatal("a refused edit reached Paprika")
	}
}
