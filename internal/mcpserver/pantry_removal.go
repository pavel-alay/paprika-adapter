package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
)

type ProposePantryRemovalIn struct {
	ItemUIDs []string `json:"item_uids" jsonschema:"uids of the 1 to 25 pantry items to remove, from list_pantry"`
}

func (s *server) proposePantryRemoval(ctx context.Context, in ProposePantryRemovalIn, out *ProposeOut) *ToolError {
	if len(in.ItemUIDs) == 0 || len(in.ItemUIDs) > maxPantryItems {
		return invalid(fmt.Sprintf("item_uids must hold 1 to %d entries; get them from list_pantry", maxPantryItems))
	}
	if terr := s.ensureFresh(ctx, &out.Meta, string(paprika.Pantry)); terr != nil {
		return terr
	}
	existing, err := store.Load[paprika.PantryItem](ctx, s.Store, paprika.Pantry)
	if err != nil {
		return fromErr(err)
	}
	var p payload
	var lines []string
	seen := map[string]bool{}
	for i, raw := range in.ItemUIDs {
		uid := strings.TrimSpace(raw)
		if uid == "" {
			return invalid(fmt.Sprintf("item_uids[%d] is empty", i))
		}
		if seen[uid] {
			continue
		}
		seen[uid] = true
		item, ok := findPantry(existing, uid)
		if !ok {
			return notFound("no pantry item with uid " + uid + "; see list_pantry")
		}
		p.PantryRemoval = append(p.PantryRemoval, pantryEntry(item))
		lines = append(lines, strings.TrimSpace(item.Quantity+" "+item.Ingredient))
	}
	p.Summary = fmt.Sprintf("REMOVE %d item(s) from the Paprika pantry: %s. They disappear from the pantry on every device.",
		len(p.PantryRemoval), strings.Join(lines, "; "))
	view, terr := s.saveProposal(ctx, kindPantryRemoval, p)
	if terr != nil {
		return terr
	}
	view.PantryRemoved = p.PantryRemoval
	out.Proposal = view
	return nil
}

// commitPantryRemoval removes the proposed items that Paprika still holds and
// checks that none of them is returned any more.
func (s *server) commitPantryRemoval(ctx context.Context, p payload, out *CommitOut) *ToolError {
	raws, _, terr := s.pullPantry(ctx)
	if terr != nil {
		return terr
	}
	var send []paprika.Raw
	for _, it := range p.PantryRemoval {
		if raw, present := raws[it.UID]; present { // an absent one is gone already, which is the goal
			send = append(send, raw)
		}
	}
	if len(send) > 0 {
		if err := s.Client.RemovePantryItems(ctx, send); err != nil {
			return writeFailed(err)
		}
		if raws, _, terr = s.pullPantry(ctx); terr != nil {
			return notVerified(terr.Message)
		}
	}
	for _, it := range p.PantryRemoval {
		if _, present := raws[it.UID]; present {
			return notVerified("Paprika accepted the removal but still returns " + it.Ingredient)
		}
	}
	out.PantryRemoved = p.PantryRemoval
	out.Sync = s.Sync.Freshness(ctx, string(paprika.Pantry))
	return nil
}
