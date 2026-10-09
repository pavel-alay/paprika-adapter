package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
)

type ProposeMealEditIn struct {
	MealUID        string `json:"meal_uid" jsonschema:"uid of the meal-plan entry to change, from get_meal_plan"`
	Date           string `json:"date,omitempty" jsonschema:"move the entry to this day, YYYY-MM-DD"`
	MealTypeUID    string `json:"meal_type_uid,omitempty" jsonschema:"change the meal type; a uid from get_meal_plan.meal_types"`
	RecipeUID      string `json:"recipe_uid,omitempty" jsonschema:"plan this recipe in place of what the entry has; not together with name"`
	Name           string `json:"name,omitempty" jsonschema:"plan this free text, such as Leftovers, in place of what the entry has; not together with recipe_uid"`
	AllowDuplicate bool   `json:"allow_duplicate,omitempty" jsonschema:"allow the result to repeat a recipe or text already planned for that day and meal type; default false"`
}

// mealEdit is what a proposal stores.
type mealEdit struct {
	UID    string           `json:"uid"`
	Edit   paprika.MealEdit `json:"edit"`
	Before MealEntry        `json:"before"` // as shown to the user when proposing
}

func (s *server) proposeMealEdit(ctx context.Context, in ProposeMealEditIn, out *ProposeOut) *ToolError {
	uid, recipeUID, name := strings.TrimSpace(in.MealUID), strings.TrimSpace(in.RecipeUID), strings.TrimSpace(in.Name)
	typeUID, date := strings.TrimSpace(in.MealTypeUID), strings.TrimSpace(in.Date)
	if uid == "" {
		return invalid("meal_uid is required; get it from get_meal_plan")
	}
	if recipeUID != "" && name != "" {
		return invalid("give recipe_uid or name, not both")
	}
	if date == "" && typeUID == "" && recipeUID == "" && name == "" {
		return invalid("give at least one of date, meal_type_uid, recipe_uid, name")
	}
	if terr := checkText("name", name); terr != nil {
		return terr
	}
	edit := mealEdit{UID: uid}
	if date != "" {
		day, terr := s.planDay(date)
		if terr != nil {
			return terr
		}
		stamp := day.Format(dayLayout) + " 00:00:00"
		edit.Edit.Date = &stamp
	}
	if terr := s.ensureFresh(ctx, &out.Meta, string(paprika.Meals), string(paprika.MealTypes)); terr != nil {
		return terr
	}
	types, err := s.mealTypes(ctx)
	if err != nil {
		return fromErr(err)
	}
	if typeUID != "" {
		t, ok := types.byUID[typeUID]
		if !ok {
			return notFound("no meal type with uid " + typeUID + "; see get_meal_plan.meal_types")
		}
		edit.Edit.Type = &paprika.MealSlot{UID: t.UID}
		if t.OriginalType != nil {
			edit.Edit.Type.Type = *t.OriginalType
		}
	}
	switch {
	case recipeUID != "":
		recipeName, terr := s.plannable(ctx, recipeUID)
		if terr != nil {
			return terr
		}
		edit.Edit.Subject = &paprika.MealSubject{RecipeUID: &recipeUID, Name: recipeName}
	case name != "":
		edit.Edit.Subject = &paprika.MealSubject{Name: name}
	}
	existing, err := store.Load[paprika.Meal](ctx, s.Store, paprika.Meals)
	if err != nil {
		return fromErr(err)
	}
	current, ok := findMeal(existing, uid)
	if !ok {
		return notFound("no meal-plan entry with uid " + uid + "; see get_meal_plan")
	}
	after := applyMealEdit(current, edit.Edit)
	before, result := types.entry(current), types.entry(after)
	if result == before {
		return &ToolError{Code: "duplicate", Message: "nothing to change: the entry already is as requested"}
	}
	if dup := duplicateMeal(existing, asNewMeal(after)); dup != nil && !in.AllowDuplicate {
		return &ToolError{Code: "duplicate", Message: fmt.Sprintf("%q is already planned as %s on %s (meal uid %s); pass allow_duplicate=true to plan it twice",
			after.Name, result.MealType, result.Date, dup.UID)}
	}
	var parts []string
	if result.Date != before.Date {
		parts = append(parts, "move it to "+dayWithWeekday(result.Date))
	}
	if result.MealTypeUID != before.MealTypeUID {
		parts = append(parts, "make it "+result.MealType)
	}
	if result.Name != before.Name || result.RecipeUID != before.RecipeUID {
		parts = append(parts, fmt.Sprintf("plan %q instead", result.Name))
	}
	edit.Before = before
	p := payload{MealEdit: &edit, AllowDuplicates: in.AllowDuplicate, Summary: fmt.Sprintf("Change the Paprika meal plan entry %q (%s on %s): %s. This overwrites the entry.",
		before.Name, before.MealType, dayWithWeekday(before.Date), strings.Join(parts, "; "))}
	view, terr := s.saveProposal(ctx, kindMealEdit, p)
	if terr != nil {
		return terr
	}
	view.Meal, view.MealBefore = &result, &before
	out.Proposal = view
	return nil
}

// commitMealEdit reads the meal plan from Paprika, overwrites the edited fields
// of the entry on that copy and reads it back.
func (s *server) commitMealEdit(ctx context.Context, p payload, out *CommitOut) *ToolError {
	edit := *p.MealEdit
	raw, meals, terr := s.pullMeal(ctx, edit.UID)
	if terr != nil {
		return terr
	}
	current, _ := findMeal(meals, edit.UID)
	want := applyMealEdit(current, edit.Edit)
	if !sameMeal(current, want) { // otherwise an earlier attempt already landed
		if dup := duplicateMeal(meals, asNewMeal(want)); dup != nil && !p.AllowDuplicates {
			return &ToolError{Code: "duplicate", Message: "nothing was written: the same entry was added to the plan in the meantime (meal uid " + dup.UID + ")"}
		}
		if err := s.Client.EditMeal(ctx, raw, edit.Edit); err != nil {
			return writeFailed(err)
		}
		if _, meals, terr = s.pullMeal(ctx, edit.UID); terr != nil {
			return notVerified(terr.Message)
		}
	}
	got, _ := findMeal(meals, edit.UID)
	if !sameMeal(got, want) {
		return notVerified("Paprika accepted the entry but still returns it unchanged")
	}
	types, err := s.mealTypes(ctx)
	if err != nil {
		return fromErr(err)
	}
	entry := types.entry(got)
	out.Meal, out.MealBefore = &entry, &edit.Before
	out.Sync = s.Sync.Freshness(ctx, string(paprika.Meals))
	return nil
}

// pullMeal reads the meal plan from Paprika and returns the raw row of one entry with it.
func (s *server) pullMeal(ctx context.Context, uid string) (paprika.Raw, []paprika.Meal, *ToolError) {
	rows, err := s.Sync.Pull(ctx, paprika.Meals, nil)
	if err != nil {
		return paprika.Raw{}, nil, fromErr(err)
	}
	meals := make([]paprika.Meal, 0, len(rows))
	var raw paprika.Raw
	for _, r := range rows {
		m, err := paprika.Decode[paprika.Meal](r.JSON)
		if err != nil {
			return paprika.Raw{}, nil, fromErr(err)
		}
		meals = append(meals, m)
		if r.UID == uid {
			raw = r
		}
	}
	if raw.UID == "" {
		return raw, nil, notFound("the meal-plan entry is no longer in Paprika; it was removed in the meantime")
	}
	return raw, meals, nil
}

func applyMealEdit(m paprika.Meal, e paprika.MealEdit) paprika.Meal {
	if e.Date != nil {
		m.Date = *e.Date
	}
	if e.Type != nil {
		uid := e.Type.UID
		m.TypeUID, m.Type = &uid, e.Type.Type
	}
	if e.Subject != nil {
		m.RecipeUID, m.Name = e.Subject.RecipeUID, e.Subject.Name
	}
	return m
}

func sameMeal(a, b paprika.Meal) bool {
	return dateOnly(a.Date) == dateOnly(b.Date) && deref(a.TypeUID) == deref(b.TypeUID) && a.Type == b.Type &&
		deref(a.RecipeUID) == deref(b.RecipeUID) && a.Name == b.Name
}

func asNewMeal(m paprika.Meal) paprika.NewMeal {
	return paprika.NewMeal{UID: m.UID, RecipeUID: m.RecipeUID, Name: m.Name, Date: m.Date, Type: m.Type, TypeUID: deref(m.TypeUID)}
}

func dayWithWeekday(date string) string {
	day, err := time.Parse(dayLayout, date)
	if err != nil {
		return date
	}
	return fmt.Sprintf("%s (%s)", date, day.Weekday())
}
