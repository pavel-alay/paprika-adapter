package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
	"paprika-adapter/internal/syncer"
)

const (
	kindMeal      = "meal_plan_entry"
	kindGroceries = "grocery_items"
	kindPantry    = "pantry_items"
	kindRecipe    = "recipe_edit"
	kindMealEdit  = "meal_plan_edit"

	kindPantryEdit    = "pantry_edit"
	kindPantryRemoval = "pantry_removal"

	maxGroceryItems = 25
	maxTextLen      = 200
	maxPlanDistance = 366 * 24 * time.Hour
)

// payload is what a proposal stores: the exact rows a commit will send.
type payload struct {
	Summary         string                   `json:"summary"`
	Meal            *paprika.NewMeal         `json:"meal,omitempty"`
	List            *NamedRef                `json:"list,omitempty"`
	Items           []paprika.NewGroceryItem `json:"items,omitempty"`
	Pantry          []paprika.NewPantryItem  `json:"pantry,omitempty"`
	Recipe          *recipeEdit              `json:"recipe,omitempty"`
	MealEdit        *mealEdit                `json:"meal_edit,omitempty"`
	PantryEdit      *pantryEdit              `json:"pantry_edit,omitempty"`
	PantryRemoval   []PantryEntry            `json:"pantry_removal,omitempty"` // as shown to the user when proposing
	Skipped         []SkippedItem            `json:"skipped,omitempty"`
	AllowDuplicates bool                     `json:"allow_duplicates"`
}

type ProposalView struct {
	ProposalID    string          `json:"proposal_id" jsonschema:"pass this to commit_proposal"`
	Kind          string          `json:"kind" jsonschema:"meal_plan_entry, meal_plan_edit, grocery_items, pantry_items, pantry_edit, pantry_removal or recipe_edit"`
	ExpiresAt     string          `json:"expires_at" jsonschema:"RFC 3339 UTC time after which the proposal can no longer be committed"`
	Summary       string          `json:"summary" jsonschema:"plain-language statement of exactly what commit_proposal will write; show it to the user and wait for approval"`
	Meal          *MealEntry      `json:"meal,omitempty" jsonschema:"the meal-plan entry that would be created, or how an edited entry would look afterwards"`
	MealBefore    *MealEntry      `json:"meal_before,omitempty" jsonschema:"for an edit: the entry as it is now"`
	List          *NamedRef       `json:"list,omitempty" jsonschema:"the grocery list that would receive the items"`
	Items         []ProposedItem  `json:"items,omitempty" jsonschema:"the grocery items that would be created"`
	Pantry        []PantryEntry   `json:"pantry_items,omitempty" jsonschema:"the pantry items that would be created, or how an edited item would look afterwards"`
	PantryBefore  []PantryEntry   `json:"pantry_before,omitempty" jsonschema:"for an edit: the item as it is now"`
	PantryRemoved []PantryEntry   `json:"pantry_removed,omitempty" jsonschema:"the pantry items that would be removed"`
	Recipe        *RecipeEditView `json:"recipe_edit,omitempty" jsonschema:"the recipe that would be changed, with the old and new value of every field"`
	Skipped       []SkippedItem   `json:"skipped,omitempty" jsonschema:"requested items that will NOT be written, with the reason"`
}

type ProposedItem struct {
	UID         string `json:"uid" jsonschema:"uid the item will have in Paprika"`
	Name        string `json:"name"`
	Quantity    string `json:"quantity,omitempty"`
	Instruction string `json:"instruction,omitempty"`
	Aisle       string `json:"aisle,omitempty" jsonschema:"aisle Paprika already associates with this ingredient, if any"`
}

type SkippedItem struct {
	Name        string `json:"name"`
	Reason      string `json:"reason"`
	ExistingUID string `json:"existing_uid,omitempty" jsonschema:"uid of the item already in Paprika"`
}

type ProposeOut struct {
	Meta
	Proposal *ProposalView `json:"proposal,omitempty"`
}

type ProposeMealIn struct {
	Date           string `json:"date" jsonschema:"day to plan, YYYY-MM-DD"`
	MealTypeUID    string `json:"meal_type_uid" jsonschema:"uid of the meal type (Breakfast, Lunch, Dinner, ...) from get_meal_plan.meal_types"`
	RecipeUID      string `json:"recipe_uid,omitempty" jsonschema:"recipe to plan; give either recipe_uid or name, not both"`
	Name           string `json:"name,omitempty" jsonschema:"free-text entry such as Leftovers, for a meal that is not a recipe"`
	AllowDuplicate bool   `json:"allow_duplicate,omitempty" jsonschema:"plan the same recipe or text again for the same date and meal type; default false"`
}

func (s *server) proposeMeal(ctx context.Context, in ProposeMealIn, out *ProposeOut) *ToolError {
	day, terr := s.planDay(in.Date)
	if terr != nil {
		return terr
	}
	recipeUID, name := strings.TrimSpace(in.RecipeUID), strings.TrimSpace(in.Name)
	if (recipeUID == "") == (name == "") {
		return invalid("give exactly one of recipe_uid or name")
	}
	if terr := checkText("name", name); terr != nil {
		return terr
	}
	if strings.TrimSpace(in.MealTypeUID) == "" {
		return invalid("meal_type_uid is required; get it from get_meal_plan.meal_types")
	}
	if terr := s.ensureFresh(ctx, &out.Meta, string(paprika.Meals), string(paprika.MealTypes)); terr != nil {
		return terr
	}
	types, err := s.mealTypes(ctx)
	if err != nil {
		return fromErr(err)
	}
	mealType, ok := types.byUID[strings.TrimSpace(in.MealTypeUID)]
	if !ok {
		return notFound("no meal type with uid " + in.MealTypeUID + "; see get_meal_plan.meal_types")
	}
	meal := paprika.NewMeal{UID: paprika.NewUID(), Name: name, Date: day.Format(dayLayout) + " 00:00:00", TypeUID: mealType.UID}
	if mealType.OriginalType != nil {
		meal.Type = *mealType.OriginalType
	}
	if recipeUID != "" {
		recipeName, terr := s.plannable(ctx, recipeUID)
		if terr != nil {
			return terr
		}
		meal.RecipeUID, meal.Name = &recipeUID, recipeName
	}
	existing, err := store.Load[paprika.Meal](ctx, s.Store, paprika.Meals)
	if err != nil {
		return fromErr(err)
	}
	if dup := duplicateMeal(existing, meal); dup != nil && !in.AllowDuplicate {
		return &ToolError{Code: "duplicate", Message: fmt.Sprintf("%q is already planned as %s on %s (meal uid %s); pass allow_duplicate=true to plan it again",
			meal.Name, mealType.Name, day.Format(dayLayout), dup.UID)}
	}
	for _, m := range existing {
		if dateOnly(m.Date) == day.Format(dayLayout) && m.OrderFlag >= meal.OrderFlag {
			meal.OrderFlag = m.OrderFlag + 1
		}
	}
	entry := MealEntry{UID: meal.UID, Date: day.Format(dayLayout), MealType: mealType.Name, MealTypeUID: mealType.UID, Name: meal.Name, RecipeUID: recipeUID}
	p := payload{
		Summary:         fmt.Sprintf("Add %q as %s on %s (%s) to the Paprika meal plan.", meal.Name, mealType.Name, entry.Date, day.Weekday()),
		Meal:            &meal,
		AllowDuplicates: in.AllowDuplicate,
	}
	view, terr := s.saveProposal(ctx, kindMeal, p)
	if terr != nil {
		return terr
	}
	view.Meal = &entry
	out.Proposal = view
	return nil
}

type GroceryInput struct {
	Name        string `json:"name" jsonschema:"what to buy, without the amount, e.g. eggs"`
	Quantity    string `json:"quantity,omitempty" jsonschema:"amount as free text, e.g. 1 dozen or 500 g"`
	Instruction string `json:"instruction,omitempty" jsonschema:"note shown with the item, e.g. the organic ones"`
}

type ProposeGroceriesIn struct {
	ListUID         string         `json:"list_uid" jsonschema:"uid of the grocery list to add to, from list_grocery_lists; there is no default list"`
	Items           []GroceryInput `json:"items" jsonschema:"1 to 25 items to add"`
	AllowDuplicates bool           `json:"allow_duplicates,omitempty" jsonschema:"add items even if an unpurchased item with the same name is already on the list; default false"`
}

func (s *server) proposeGroceries(ctx context.Context, in ProposeGroceriesIn, out *ProposeOut) *ToolError {
	if strings.TrimSpace(in.ListUID) == "" {
		return invalid("list_uid is required; get it from list_grocery_lists")
	}
	if len(in.Items) == 0 || len(in.Items) > maxGroceryItems {
		return invalid(fmt.Sprintf("items must hold 1 to %d entries", maxGroceryItems))
	}
	for i, it := range in.Items {
		if strings.TrimSpace(it.Name) == "" {
			return invalid(fmt.Sprintf("items[%d].name is required", i))
		}
		for field, v := range map[string]string{"name": it.Name, "quantity": it.Quantity, "instruction": it.Instruction} {
			if terr := checkText(fmt.Sprintf("items[%d].%s", i, field), v); terr != nil {
				return terr
			}
		}
	}
	if terr := s.ensureFresh(ctx, &out.Meta, string(paprika.GroceryLists), string(paprika.Groceries)); terr != nil {
		return terr
	}
	_ = s.Sync.Ensure(ctx, string(paprika.GroceryAisles), string(paprika.GroceryIngredients)) // aisle hints are optional
	list, terr := s.groceryList(ctx, strings.TrimSpace(in.ListUID))
	if terr != nil {
		return terr
	}
	existing, err := store.Load[paprika.GroceryItem](ctx, s.Store, paprika.Groceries)
	if err != nil {
		return fromErr(err)
	}
	aisles, err := s.aisleHints(ctx)
	if err != nil {
		return fromErr(err)
	}
	order := 0
	for _, e := range existing {
		if e.ListUID == list.UID && e.OrderFlag >= order {
			order = e.OrderFlag + 1
		}
	}
	p := payload{List: list, AllowDuplicates: in.AllowDuplicates}
	var shown []ProposedItem
	var lines []string
	seen := map[string]bool{}
	for _, raw := range in.Items {
		name, qty := strings.TrimSpace(raw.Name), strings.TrimSpace(raw.Quantity)
		item := paprika.NewGroceryItem{
			UID: paprika.NewUID(), ListUID: list.UID, Name: strings.TrimSpace(qty + " " + name), Ingredient: name,
			Quantity: qty, Instruction: strings.TrimSpace(raw.Instruction), OrderFlag: order,
		}
		if seen[norm(name)] {
			p.Skipped = append(p.Skipped, SkippedItem{Name: name, Reason: "listed twice in this request"})
			continue
		}
		seen[norm(name)] = true
		if dup := duplicateGrocery(existing, item); dup != nil && !in.AllowDuplicates {
			p.Skipped = append(p.Skipped, SkippedItem{Name: name, Reason: "already on the list, not yet purchased", ExistingUID: dup.UID})
			continue
		}
		if a, ok := aisles.forIngredient(name); ok {
			item.Aisle, item.AisleUID = a.Name, &a.UID
		}
		order++
		p.Items = append(p.Items, item)
		shown = append(shown, ProposedItem{UID: item.UID, Name: item.Name, Quantity: qty, Instruction: item.Instruction, Aisle: item.Aisle})
		lines = append(lines, item.Name)
	}
	if len(p.Items) == 0 {
		return &ToolError{Code: "duplicate", Message: fmt.Sprintf("nothing to add: every requested item is already on %q; pass allow_duplicates=true to add them anyway", list.Name)}
	}
	p.Summary = fmt.Sprintf("Add %d item(s) to the Paprika grocery list %q: %s.", len(p.Items), list.Name, strings.Join(lines, "; "))
	if len(p.Skipped) > 0 {
		var names []string
		for _, sk := range p.Skipped {
			names = append(names, sk.Name)
		}
		p.Summary += " Not added because already there: " + strings.Join(names, "; ") + "."
	}
	view, terr := s.saveProposal(ctx, kindGroceries, p)
	if terr != nil {
		return terr
	}
	view.List, view.Items, view.Skipped = list, shown, p.Skipped
	out.Proposal = view
	return nil
}

// planDay parses a day of the meal plan and refuses one implausibly far from today.
func (s *server) planDay(date string) (time.Time, *ToolError) {
	day, err := time.Parse(dayLayout, strings.TrimSpace(date))
	if err != nil {
		return day, invalid("date must look like 2026-10-09")
	}
	now := s.Now().In(s.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if d := day.Sub(today); d > maxPlanDistance || d < -maxPlanDistance {
		return day, invalid("date is more than a year away from today (" + today.Format(dayLayout) + "); check the year")
	}
	return day, nil
}

// plannable returns the name of a recipe that may be put on the meal plan.
func (s *server) plannable(ctx context.Context, recipeUID string) (string, *ToolError) {
	if terr := s.ensure(ctx, &Meta{}, paprika.Recipes); terr != nil {
		return "", terr
	}
	cached, ok, err := s.Store.Recipe(ctx, recipeUID)
	if err != nil {
		return "", fromErr(err)
	}
	if !ok {
		return "", notFound("no recipe with uid " + recipeUID)
	}
	recipe, err := paprika.Decode[paprika.Recipe](cached.Raw)
	if err != nil {
		return "", fromErr(err)
	}
	if recipe.InTrash {
		return "", invalid("recipe " + recipe.Name + " is in Paprika's trash")
	}
	return recipe.Name, nil
}

func (s *server) saveProposal(ctx context.Context, kind string, p payload) (*ProposalView, *ToolError) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fromErr(err)
	}
	now := s.Now()
	prop := store.Proposal{ID: paprika.NewUID(), Kind: kind, Payload: raw, CreatedAt: now, ExpiresAt: now.Add(s.ProposalTTL)}
	if err := s.Store.SaveProposal(ctx, prop); err != nil {
		return nil, fromErr(err)
	}
	return &ProposalView{ProposalID: prop.ID, Kind: kind, ExpiresAt: prop.ExpiresAt.UTC().Format(time.RFC3339), Summary: p.Summary}, nil
}

// ensureFresh is ensure for writes: a write is never prepared from data that
// could not be confirmed against Paprika just now.
func (s *server) ensureFresh(ctx context.Context, m *Meta, collections ...string) *ToolError {
	if terr := s.ensure(ctx, m, collections...); terr != nil {
		return terr
	}
	for _, f := range m.Sync {
		if f.State == syncer.StateFresh {
			continue
		}
		terr := &ToolError{Code: string(paprika.KindUnavailable), Retryable: true,
			Message: "cached " + f.Collection + " could not be confirmed against Paprika, so no write is prepared from it"}
		if f.Error != nil {
			terr.Code, terr.Message = f.Error.Code, terr.Message+": "+f.Error.Message
			terr.Retryable = f.Error.Code == string(paprika.KindUnavailable)
		}
		return terr
	}
	return nil
}

func checkText(field, v string) *ToolError {
	if len(v) > maxTextLen {
		return invalid(fmt.Sprintf("%s is longer than %d characters", field, maxTextLen))
	}
	if strings.ContainsAny(v, "\r\n\x00") {
		return invalid(field + " must be a single line")
	}
	return nil
}

func norm(v string) string { return strings.Join(strings.Fields(strings.ToLower(v)), " ") }

func duplicateMeal(existing []paprika.Meal, m paprika.NewMeal) *paprika.Meal {
	for i, e := range existing {
		if dateOnly(e.Date) != dateOnly(m.Date) || deref(e.TypeUID) != m.TypeUID || e.UID == m.UID {
			continue
		}
		if m.RecipeUID != nil && deref(e.RecipeUID) == *m.RecipeUID {
			return &existing[i]
		}
		if m.RecipeUID == nil && e.RecipeUID == nil && norm(e.Name) == norm(m.Name) {
			return &existing[i]
		}
	}
	return nil
}

func duplicateGrocery(existing []paprika.GroceryItem, it paprika.NewGroceryItem) *paprika.GroceryItem {
	want := norm(it.Ingredient)
	for i, e := range existing {
		if e.ListUID != it.ListUID || e.Purchased || e.UID == it.UID {
			continue
		}
		if norm(e.Ingredient) == want || norm(e.Name) == want || norm(e.Name) == norm(it.Name) {
			return &existing[i]
		}
	}
	return nil
}

// aisleIndex suggests the aisle Paprika already remembers for an ingredient.
type aisleIndex struct {
	byIngredient map[string]string // normalised ingredient name -> aisle uid
	byUID        map[string]paprika.GroceryAisle
}

func (s *server) aisleHints(ctx context.Context) (aisleIndex, error) {
	idx := aisleIndex{byIngredient: map[string]string{}, byUID: map[string]paprika.GroceryAisle{}}
	aisles, err := store.Load[paprika.GroceryAisle](ctx, s.Store, paprika.GroceryAisles)
	if err != nil {
		return idx, err
	}
	for _, a := range aisles {
		idx.byUID[a.UID] = a
	}
	known, err := store.Load[paprika.GroceryIngredient](ctx, s.Store, paprika.GroceryIngredients)
	if err != nil {
		return idx, err
	}
	for _, k := range known {
		if uid := deref(k.AisleUID); uid != "" {
			idx.byIngredient[norm(k.Name)] = uid
		}
	}
	return idx, nil
}

func (i aisleIndex) forIngredient(name string) (paprika.GroceryAisle, bool) {
	a, ok := i.byUID[i.byIngredient[norm(name)]]
	return a, ok
}
