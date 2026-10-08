package mcpserver

import (
	"context"
	"sort"
	"strings"
	"time"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
)

type ListPantryIn struct {
	Query       string `json:"query,omitempty" jsonschema:"only items whose ingredient contains this text, case-insensitive"`
	InStockOnly bool   `json:"in_stock_only,omitempty" jsonschema:"leave out items marked as not in stock"`
	Limit       int    `json:"limit,omitempty" jsonschema:"maximum items to return, 1 to 500, default 100"`
}

type PantryEntry struct {
	UID            string `json:"uid" jsonschema:"stable Paprika pantry item uid"`
	Ingredient     string `json:"ingredient"`
	Quantity       string `json:"quantity,omitempty" jsonschema:"free text as entered in Paprika"`
	Aisle          string `json:"aisle,omitempty"`
	InStock        bool   `json:"in_stock"`
	PurchaseDate   string `json:"purchase_date,omitempty" jsonschema:"YYYY-MM-DD"`
	ExpirationDate string `json:"expiration_date,omitempty" jsonschema:"YYYY-MM-DD, present only when the item has an expiration date"`
}

type ListPantryOut struct {
	Meta
	Items     []PantryEntry `json:"items,omitzero" jsonschema:"sorted by ingredient"`
	Total     int           `json:"total" jsonschema:"items matching the filters before limit"`
	Truncated bool          `json:"truncated"`
}

func (s *server) listPantry(ctx context.Context, in ListPantryIn, out *ListPantryOut) *ToolError {
	if terr := s.ensure(ctx, &out.Meta, string(paprika.Pantry)); terr != nil {
		return terr
	}
	items, err := store.Load[paprika.PantryItem](ctx, s.Store, paprika.Pantry)
	if err != nil {
		return fromErr(err)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Ingredient) < strings.ToLower(items[j].Ingredient)
	})
	needle, limit := strings.ToLower(strings.TrimSpace(in.Query)), clamp(in.Limit, 100, 500)
	out.Items = []PantryEntry{}
	for _, p := range items {
		if (in.InStockOnly && !p.InStock) || !strings.Contains(strings.ToLower(p.Ingredient), needle) {
			continue
		}
		out.Total++
		if len(out.Items) == limit {
			continue
		}
		e := PantryEntry{UID: p.UID, Ingredient: p.Ingredient, Quantity: p.Quantity, Aisle: p.Aisle, InStock: p.InStock,
			PurchaseDate: dateOnly(deref(p.PurchaseDate))}
		if p.HasExpiration {
			e.ExpirationDate = dateOnly(deref(p.ExpirationDate))
		}
		out.Items = append(out.Items, e)
	}
	out.Truncated = out.Total > len(out.Items)
	return nil
}

type GetMealPlanIn struct {
	From string `json:"from,omitempty" jsonschema:"first day, YYYY-MM-DD; default is Monday of the current week"`
	To   string `json:"to,omitempty" jsonschema:"last day inclusive, YYYY-MM-DD; default is six days after from"`
}

type MealEntry struct {
	UID         string `json:"uid" jsonschema:"stable Paprika meal uid"`
	Date        string `json:"date" jsonschema:"YYYY-MM-DD"`
	MealType    string `json:"meal_type" jsonschema:"meal type name, e.g. Dinner; empty if Paprika no longer has the type"`
	MealTypeUID string `json:"meal_type_uid,omitempty"`
	Name        string `json:"name" jsonschema:"recipe name, or the free text of a note-style entry"`
	RecipeUID   string `json:"recipe_uid,omitempty" jsonschema:"set when the entry points at a recipe; pass it to get_recipe"`
}

type GetMealPlanOut struct {
	Meta
	From      string      `json:"from,omitempty" jsonschema:"first day covered, YYYY-MM-DD"`
	To        string      `json:"to,omitempty" jsonschema:"last day covered, YYYY-MM-DD"`
	Today     string      `json:"today,omitempty" jsonschema:"current date in timezone, YYYY-MM-DD"`
	Timezone  string      `json:"timezone,omitempty" jsonschema:"zone used to decide today and the default week"`
	Meals     []MealEntry `json:"meals,omitzero" jsonschema:"sorted by date, then meal type order"`
	MealTypes []NamedRef  `json:"meal_types,omitzero" jsonschema:"meal types defined in Paprika; use a uid as meal_type_uid when proposing an entry"`
}

const (
	dayLayout    = "2006-01-02"
	maxPlanRange = 92 * 24 * time.Hour
)

func (s *server) getMealPlan(ctx context.Context, in GetMealPlanIn, out *GetMealPlanOut) *ToolError {
	now := s.Now().In(s.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	var err error
	if in.From != "" {
		if from, err = time.Parse(dayLayout, in.From); err != nil {
			return invalid("from must be a date like 2026-10-05")
		}
	}
	to := from.AddDate(0, 0, 6)
	if in.To != "" {
		if to, err = time.Parse(dayLayout, in.To); err != nil {
			return invalid("to must be a date like 2026-10-11")
		}
	}
	if to.Before(from) || to.Sub(from) > maxPlanRange {
		return invalid("to must be on or after from, and at most 92 days later")
	}
	if terr := s.ensure(ctx, &out.Meta, string(paprika.Meals), string(paprika.MealTypes)); terr != nil {
		return terr
	}
	meals, err := store.Load[paprika.Meal](ctx, s.Store, paprika.Meals)
	if err != nil {
		return fromErr(err)
	}
	types, err := s.mealTypes(ctx)
	if err != nil {
		return fromErr(err)
	}
	out.From, out.To = from.Format(dayLayout), to.Format(dayLayout)
	out.Today, out.Timezone = today.Format(dayLayout), s.Location.String()
	out.MealTypes = make([]NamedRef, 0, len(types.ordered))
	for _, t := range types.ordered {
		out.MealTypes = append(out.MealTypes, NamedRef{UID: t.UID, Name: t.Name})
	}
	sort.SliceStable(meals, func(i, j int) bool {
		a, b := meals[i], meals[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if oa, ob := types.order(a), types.order(b); oa != ob {
			return oa < ob
		}
		return a.OrderFlag < b.OrderFlag
	})
	out.Meals = []MealEntry{}
	for _, m := range meals {
		if day := dateOnly(m.Date); day >= out.From && day <= out.To {
			out.Meals = append(out.Meals, types.entry(m))
		}
	}
	return nil
}

// mealTypeIndex resolves a meal's type. Meals written before Paprika had custom
// meal types carry only the numeric type, so both lookups are needed.
type mealTypeIndex struct {
	ordered    []paprika.MealType
	byUID      map[string]paprika.MealType
	byOriginal map[int]paprika.MealType
}

func (s *server) mealTypes(ctx context.Context) (mealTypeIndex, error) {
	types, err := store.Load[paprika.MealType](ctx, s.Store, paprika.MealTypes)
	if err != nil {
		return mealTypeIndex{}, err
	}
	sort.SliceStable(types, func(i, j int) bool { return types[i].OrderFlag < types[j].OrderFlag })
	idx := mealTypeIndex{ordered: types, byUID: map[string]paprika.MealType{}, byOriginal: map[int]paprika.MealType{}}
	for _, t := range types {
		idx.byUID[t.UID] = t
		if t.OriginalType != nil {
			idx.byOriginal[*t.OriginalType] = t
		}
	}
	return idx, nil
}

func (i mealTypeIndex) of(m paprika.Meal) (paprika.MealType, bool) {
	if m.TypeUID != nil && *m.TypeUID != "" {
		t, ok := i.byUID[*m.TypeUID]
		return t, ok
	}
	t, ok := i.byOriginal[m.Type]
	return t, ok
}

func (i mealTypeIndex) order(m paprika.Meal) int {
	if t, ok := i.of(m); ok {
		return t.OrderFlag
	}
	return 1 << 30
}

func (i mealTypeIndex) entry(m paprika.Meal) MealEntry {
	t, _ := i.of(m)
	return MealEntry{UID: m.UID, Date: dateOnly(m.Date), MealType: t.Name, MealTypeUID: t.UID, Name: m.Name, RecipeUID: deref(m.RecipeUID)}
}
