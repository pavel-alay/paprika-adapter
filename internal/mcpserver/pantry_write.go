package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
)

const maxPantryItems = 25

type PantryInput struct {
	Ingredient     string `json:"ingredient" jsonschema:"what is now in the pantry, without the amount, e.g. basmati rice"`
	Quantity       string `json:"quantity,omitempty" jsonschema:"amount as free text, e.g. 2 kg or 3 cans"`
	ExpirationDate string `json:"expiration_date,omitempty" jsonschema:"YYYY-MM-DD, only when the item has an expiration date"`
}

type ProposePantryIn struct {
	Items           []PantryInput `json:"items" jsonschema:"1 to 25 items to add"`
	AllowDuplicates bool          `json:"allow_duplicates,omitempty" jsonschema:"add items even if the pantry already holds an item with the same ingredient; default false"`
}

func (s *server) proposePantry(ctx context.Context, in ProposePantryIn, out *ProposeOut) *ToolError {
	if len(in.Items) == 0 || len(in.Items) > maxPantryItems {
		return invalid(fmt.Sprintf("items must hold 1 to %d entries", maxPantryItems))
	}
	for i, it := range in.Items {
		if strings.TrimSpace(it.Ingredient) == "" {
			return invalid(fmt.Sprintf("items[%d].ingredient is required", i))
		}
		for field, v := range map[string]string{"ingredient": it.Ingredient, "quantity": it.Quantity} {
			if terr := checkText(fmt.Sprintf("items[%d].%s", i, field), v); terr != nil {
				return terr
			}
		}
		if exp := strings.TrimSpace(it.ExpirationDate); exp != "" {
			if _, err := time.Parse(dayLayout, exp); err != nil {
				return invalid(fmt.Sprintf("items[%d].expiration_date must look like 2027-03-01", i))
			}
		}
	}
	if terr := s.ensureFresh(ctx, &out.Meta, string(paprika.Pantry)); terr != nil {
		return terr
	}
	_ = s.Sync.Ensure(ctx, string(paprika.GroceryAisles), string(paprika.GroceryIngredients)) // aisle hints are optional
	existing, err := store.Load[paprika.PantryItem](ctx, s.Store, paprika.Pantry)
	if err != nil {
		return fromErr(err)
	}
	aisles, err := s.aisleHints(ctx)
	if err != nil {
		return fromErr(err)
	}
	bought := s.Now().In(s.Location).Format(dayLayout)
	p := payload{AllowDuplicates: in.AllowDuplicates}
	var shown []PantryEntry
	var lines []string
	seen := map[string]bool{}
	for _, raw := range in.Items {
		name, qty, exp := strings.TrimSpace(raw.Ingredient), strings.TrimSpace(raw.Quantity), strings.TrimSpace(raw.ExpirationDate)
		item := paprika.NewPantryItem{UID: paprika.NewUID(), Ingredient: name, Quantity: qty, InStock: true, PurchaseDate: bought + " 00:00:00"}
		if seen[norm(name)] {
			p.Skipped = append(p.Skipped, SkippedItem{Name: name, Reason: "listed twice in this request"})
			continue
		}
		seen[norm(name)] = true
		if dup := duplicatePantry(existing, item); dup != nil && !in.AllowDuplicates {
			reason := "already in the pantry"
			if !dup.InStock {
				reason += ", marked out of stock; use propose_pantry_edit with in_stock=true instead"
			}
			p.Skipped = append(p.Skipped, SkippedItem{Name: name, Reason: reason, ExistingUID: dup.UID})
			continue
		}
		if exp != "" {
			stamp := exp + " 00:00:00"
			item.HasExpiration, item.ExpirationDate = true, &stamp
		}
		if a, ok := aisles.forIngredient(name); ok {
			item.Aisle, item.AisleUID = a.Name, a.UID
		}
		p.Pantry = append(p.Pantry, item)
		shown = append(shown, PantryEntry{UID: item.UID, Ingredient: name, Quantity: qty, Aisle: item.Aisle, InStock: true, PurchaseDate: bought, ExpirationDate: exp})
		lines = append(lines, strings.TrimSpace(qty+" "+name))
	}
	if len(p.Pantry) == 0 {
		return &ToolError{Code: "duplicate", Message: "nothing to add: every requested item is already in the pantry; pass allow_duplicates=true to add them anyway"}
	}
	p.Summary = fmt.Sprintf("Add %d item(s) to the Paprika pantry, in stock, bought %s: %s.", len(p.Pantry), bought, strings.Join(lines, "; "))
	if len(p.Skipped) > 0 {
		var names []string
		for _, sk := range p.Skipped {
			names = append(names, sk.Name)
		}
		p.Summary += " Not added because already there: " + strings.Join(names, "; ") + "."
	}
	view, terr := s.saveProposal(ctx, kindPantry, p)
	if terr != nil {
		return terr
	}
	view.Pantry, view.Skipped = shown, p.Skipped
	out.Proposal = view
	return nil
}

func (s *server) commitPantry(ctx context.Context, p payload, out *CommitOut) *ToolError {
	items, terr := pull[paprika.PantryItem](ctx, s, paprika.Pantry)
	if terr != nil {
		return terr
	}
	skipped := p.Skipped
	var expect, send []paprika.NewPantryItem
	for _, it := range p.Pantry {
		if _, present := findPantry(items, it.UID); present {
			expect = append(expect, it)
			continue
		}
		if dup := duplicatePantry(items, it); dup != nil && !p.AllowDuplicates {
			skipped = append(skipped, SkippedItem{Name: it.Ingredient, Reason: "added to the pantry in the meantime", ExistingUID: dup.UID})
			continue
		}
		expect, send = append(expect, it), append(send, it)
	}
	if len(expect) == 0 {
		return &ToolError{Code: "duplicate", Message: "nothing was written: every proposed item is in the pantry already"}
	}
	if len(send) > 0 {
		if err := s.Client.AddPantryItems(ctx, send); err != nil {
			return writeFailed(err)
		}
		if items, terr = pull[paprika.PantryItem](ctx, s, paprika.Pantry); terr != nil {
			return notVerified(terr.Message)
		}
	}
	out.Pantry = make([]PantryEntry, 0, len(expect))
	for _, want := range expect {
		got, present := findPantry(items, want.UID)
		if !present {
			return notVerified("Paprika accepted the items but does not return " + want.Ingredient + " yet")
		}
		if got.Ingredient != want.Ingredient {
			return &ToolError{Code: "uid_conflict", Message: "nothing was overwritten: Paprika already holds a different item under uid " + want.UID + "; propose again"}
		}
		out.Pantry = append(out.Pantry, pantryEntry(got))
	}
	out.Skipped = skipped
	out.Sync = s.Sync.Freshness(ctx, string(paprika.Pantry))
	return nil
}

func duplicatePantry(existing []paprika.PantryItem, it paprika.NewPantryItem) *paprika.PantryItem {
	for i, e := range existing {
		if e.UID != it.UID && norm(e.Ingredient) == norm(it.Ingredient) {
			return &existing[i]
		}
	}
	return nil
}

func findPantry(items []paprika.PantryItem, uid string) (paprika.PantryItem, bool) {
	for _, it := range items {
		if it.UID == uid {
			return it, true
		}
	}
	return paprika.PantryItem{}, false
}
