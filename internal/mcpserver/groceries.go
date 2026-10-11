package mcpserver

import (
	"context"
	"sort"
	"strings"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
	"github.com/pavel-alay/paprika-adapter/internal/store"
	"github.com/pavel-alay/paprika-adapter/internal/syncer"
)

type ListGroceryListsIn struct{}

type GroceryListEntry struct {
	UID         string `json:"uid" jsonschema:"stable Paprika grocery list uid"`
	Name        string `json:"name"`
	IsDefault   bool   `json:"is_default"`
	Items       int    `json:"items" jsonschema:"all items on the list, purchased or not"`
	Unpurchased int    `json:"unpurchased"`
}

type ListGroceryListsOut struct {
	Meta
	Lists []GroceryListEntry `json:"lists,omitzero"`
}

func (s *server) listGroceryLists(ctx context.Context, _ ListGroceryListsIn, out *ListGroceryListsOut) *ToolError {
	if terr := s.ensure(ctx, &out.Meta, string(paprika.GroceryLists), string(paprika.Groceries)); terr != nil {
		return terr
	}
	lists, err := store.Load[paprika.GroceryList](ctx, s.Store, paprika.GroceryLists)
	if err != nil {
		return fromErr(err)
	}
	items, err := store.Load[paprika.GroceryItem](ctx, s.Store, paprika.Groceries)
	if err != nil {
		return fromErr(err)
	}
	sort.SliceStable(lists, func(i, j int) bool { return lists[i].OrderFlag < lists[j].OrderFlag })
	out.Lists = make([]GroceryListEntry, 0, len(lists))
	for _, l := range lists {
		e := GroceryListEntry{UID: l.UID, Name: l.Name, IsDefault: l.IsDefault}
		for _, it := range items {
			if it.ListUID != l.UID {
				continue
			}
			e.Items++
			if !it.Purchased {
				e.Unpurchased++
			}
		}
		out.Lists = append(out.Lists, e)
	}
	return nil
}

type ListGroceryItemsIn struct {
	ListUID          string `json:"list_uid" jsonschema:"uid of the grocery list, from list_grocery_lists"`
	IncludePurchased bool   `json:"include_purchased,omitempty" jsonschema:"also return items already ticked off"`
	Limit            int    `json:"limit,omitempty" jsonschema:"maximum items to return, 1 to 500, default 200"`
}

type GroceryEntry struct {
	UID         string `json:"uid" jsonschema:"stable Paprika grocery item uid"`
	Name        string `json:"name" jsonschema:"the line as shown in Paprika, quantity included"`
	Ingredient  string `json:"ingredient,omitempty"`
	Quantity    string `json:"quantity,omitempty"`
	Instruction string `json:"instruction,omitempty"`
	Aisle       string `json:"aisle,omitempty"`
	Purchased   bool   `json:"purchased"`
	Recipe      string `json:"recipe,omitempty" jsonschema:"name of the recipe the item was added from, if any"`
	RecipeUID   string `json:"recipe_uid,omitempty"`
}

type ListGroceryItemsOut struct {
	Meta
	List      *NamedRef      `json:"list,omitempty"`
	Items     []GroceryEntry `json:"items,omitzero" jsonschema:"in list order"`
	Total     int            `json:"total" jsonschema:"items matching the filters before limit"`
	Truncated bool           `json:"truncated"`
}

func (s *server) listGroceryItems(ctx context.Context, in ListGroceryItemsIn, out *ListGroceryItemsOut) *ToolError {
	if strings.TrimSpace(in.ListUID) == "" {
		return invalid("list_uid is required; get it from list_grocery_lists")
	}
	if terr := s.ensure(ctx, &out.Meta, string(paprika.GroceryLists), string(paprika.Groceries)); terr != nil {
		return terr
	}
	list, terr := s.groceryList(ctx, in.ListUID)
	if terr != nil {
		return terr
	}
	items, err := store.Load[paprika.GroceryItem](ctx, s.Store, paprika.Groceries)
	if err != nil {
		return fromErr(err)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].OrderFlag < items[j].OrderFlag })
	out.List, out.Items = list, []GroceryEntry{}
	limit := clamp(in.Limit, 200, 500)
	for _, it := range items {
		if it.ListUID != list.UID || (it.Purchased && !in.IncludePurchased) {
			continue
		}
		out.Total++
		if len(out.Items) < limit {
			out.Items = append(out.Items, groceryEntry(it))
		}
	}
	out.Truncated = out.Total > len(out.Items)
	return nil
}

func (s *server) groceryList(ctx context.Context, uid string) (*NamedRef, *ToolError) {
	lists, err := store.Load[paprika.GroceryList](ctx, s.Store, paprika.GroceryLists)
	if err != nil {
		return nil, fromErr(err)
	}
	for _, l := range lists {
		if l.UID == uid {
			return &NamedRef{UID: l.UID, Name: l.Name}, nil
		}
	}
	return nil, notFound("no grocery list with uid " + uid)
}

func groceryEntry(it paprika.GroceryItem) GroceryEntry {
	return GroceryEntry{UID: it.UID, Name: it.Name, Ingredient: it.Ingredient, Quantity: it.Quantity, Instruction: it.Instruction,
		Aisle: it.Aisle, Purchased: it.Purchased, Recipe: deref(it.Recipe), RecipeUID: deref(it.RecipeUID)}
}

type SyncStatusIn struct {
	Refresh bool `json:"refresh,omitempty" jsonschema:"true = contact Paprika now and re-check every collection instead of reporting the last known state"`
}

type SyncStatusOut struct {
	Meta
	Healthy        bool   `json:"healthy" jsonschema:"true when every collection is fresh and no sync error or protocol drift is recorded"`
	WritesEnabled  bool   `json:"writes_enabled" jsonschema:"whether the propose and commit tools are available"`
	PaprikaHost    string `json:"paprika_host"`
	AdapterVersion string `json:"adapter_version"`
}

func (s *server) syncStatus(ctx context.Context, in SyncStatusIn, out *SyncStatusOut) *ToolError {
	if in.Refresh {
		out.Sync = s.Sync.Refresh(ctx, syncer.All()...)
	} else {
		out.Sync = s.Sync.Freshness(ctx, syncer.All()...)
	}
	out.Healthy = true
	for _, f := range out.Sync {
		if f.State != syncer.StateFresh || f.Error != nil || f.Drift != nil {
			out.Healthy = false
		}
	}
	out.WritesEnabled, out.PaprikaHost, out.AdapterVersion = s.WritesEnabled, s.Client.Host(), s.Version
	return nil
}
