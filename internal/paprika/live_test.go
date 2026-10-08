//go:build integration

package paprika

// Opt-in tests against a real Paprika account. They are the only way to know
// that the unofficial API still behaves as docs/PROTOCOL.md says.
//
//	PAPRIKA_EMAIL, PAPRIKA_PASSWORD   account to test against
//	PAPRIKA_IT_LIST_UID               a disposable grocery list whose name contains "test";
//	                                  enables the grocery write test
//	PAPRIKA_IT_MEAL_DATE              YYYY-MM-DD, a day you do not plan on; enables the meal write test
//	PAPRIKA_IT_RECIPE_UID             optional disposable recipe to plan in the meal write test
//
//	go test -tags integration -run Live -v ./internal/paprika/
//
// The write tests create one row, read it back, then soft-delete that same row
// (deleted:true, the way the Paprika apps delete) and confirm it is gone.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func liveClient(t *testing.T) *Client {
	t.Helper()
	email, password := os.Getenv("PAPRIKA_EMAIL"), os.Getenv("PAPRIKA_PASSWORD")
	if email == "" || password == "" {
		t.Skip("PAPRIKA_EMAIL and PAPRIKA_PASSWORD are not set")
	}
	c, err := New(Options{BaseURL: os.Getenv("PAPRIKA_BASE_URL"), Email: email, Password: password, UserAgent: "paprika-adapter/integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLiveReads(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	counters, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, col := range append([]string{Recipes}, collectionNames()...) {
		if _, ok := counters[col]; !ok {
			t.Errorf("status has no counter for %s", col)
		}
	}
	for _, col := range Collections {
		rows, err := c.List(ctx, col)
		if err != nil {
			t.Errorf("%s: %v", col, err)
			continue
		}
		if d := DetectDrift(string(col), rows); !d.Empty() {
			t.Errorf("%s: field drift: new %v, gone %v", col, d.Unknown, d.Missing)
		}
		t.Logf("%s: %d rows", col, len(rows))
	}
	refs, err := c.RecipeRefs(ctx)
	if err != nil {
		t.Fatalf("recipes: %v", err)
	}
	t.Logf("recipes: %d", len(refs))
	if len(refs) == 0 {
		return
	}
	rec, err := c.Recipe(ctx, refs[0].UID)
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	if d := DetectDrift(Recipes, []Raw{rec}); !d.Empty() {
		t.Errorf("recipe: field drift: new %v, gone %v", d.Unknown, d.Missing)
	}
	if _, err := c.Recipe(ctx, NewUID()); KindOf(err) != KindNotFound {
		t.Errorf("unknown recipe: got %v, want not_found", err)
	}
}

func collectionNames() []string {
	var out []string
	for _, c := range Collections {
		out = append(out, string(c))
	}
	return out
}

func TestLiveGroceryWrite(t *testing.T) {
	c := liveClient(t)
	listUID := os.Getenv("PAPRIKA_IT_LIST_UID")
	if listUID == "" {
		t.Skip("PAPRIKA_IT_LIST_UID is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	lists, err := c.List(ctx, GroceryLists)
	if err != nil {
		t.Fatal(err)
	}
	disposable := false
	for _, raw := range lists {
		l, _ := Decode[GroceryList](raw.JSON)
		if l.UID == listUID && strings.Contains(strings.ToLower(l.Name), "test") {
			disposable = true
		}
	}
	if !disposable {
		t.Fatalf("list %s does not exist or its name does not contain \"test\"; refusing to write to it", listUID)
	}

	item := NewGroceryItem{UID: NewUID(), ListUID: listUID, Name: "[adapter-it] " + time.Now().UTC().Format(time.RFC3339), Ingredient: "adapter-it"}
	if err := c.AddGroceryItems(ctx, []NewGroceryItem{item}); err != nil {
		t.Fatalf("add: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.upsert(ctx, Groceries, []groceryWire{{NewGroceryItem: item, Deleted: true}}); err != nil {
			t.Errorf("CLEANUP FAILED, delete %q from the list by hand: %v", item.Name, err)
			return
		}
		if _, found := liveFind(t, c, Groceries, item.UID); found {
			t.Errorf("soft-deleted item %q is still returned; delete it by hand", item.Name)
		}
	})
	raw, found := liveFind(t, c, Groceries, item.UID)
	if !found {
		t.Fatal("item was acknowledged but is not returned by GET /sync/groceries/")
	}
	got, err := Decode[GroceryItem](raw.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != item.Name || got.ListUID != listUID || got.Purchased {
		t.Fatalf("read back %+v, wrote %+v", got, item)
	}
	t.Logf("read back: aisle=%q aisle_uid=%v (sent empty)", got.Aisle, got.AisleUID)

	// Posting the same uid again must not create a second row.
	if err := c.AddGroceryItems(ctx, []NewGroceryItem{item}); err != nil {
		t.Fatalf("repeat add: %v", err)
	}
	rows, err := c.List(ctx, Groceries)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if g, _ := Decode[GroceryItem](r.JSON); g.Name == item.Name {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("repeated write produced %d rows, want 1", n)
	}
}

func TestLiveMealWrite(t *testing.T) {
	c := liveClient(t)
	date := os.Getenv("PAPRIKA_IT_MEAL_DATE")
	if date == "" {
		t.Skip("PAPRIKA_IT_MEAL_DATE is not set")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		t.Fatalf("PAPRIKA_IT_MEAL_DATE: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	types, err := c.List(ctx, MealTypes)
	if err != nil || len(types) == 0 {
		t.Fatalf("meal types: %d rows, err %v", len(types), err)
	}
	mt, _ := Decode[MealType](types[0].JSON)
	meal := NewMeal{UID: NewUID(), Name: "[adapter-it] " + time.Now().UTC().Format(time.RFC3339), Date: date + " 00:00:00", TypeUID: mt.UID}
	if mt.OriginalType != nil {
		meal.Type = *mt.OriginalType
	}
	if uid := os.Getenv("PAPRIKA_IT_RECIPE_UID"); uid != "" {
		raw, err := c.Recipe(ctx, uid)
		if err != nil {
			t.Fatalf("disposable recipe: %v", err)
		}
		rec, _ := Decode[Recipe](raw.JSON)
		meal.RecipeUID, meal.Name = &uid, rec.Name
	}
	if err := c.AddMeal(ctx, meal); err != nil {
		t.Fatalf("add: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.upsert(ctx, Meals, []mealWire{{NewMeal: meal, Deleted: true}}); err != nil {
			t.Errorf("CLEANUP FAILED, delete %q on %s by hand: %v", meal.Name, date, err)
			return
		}
		if _, found := liveFind(t, c, Meals, meal.UID); found {
			t.Errorf("soft-deleted meal %q is still returned; delete it by hand", meal.Name)
		}
	})
	raw, found := liveFind(t, c, Meals, meal.UID)
	if !found {
		t.Fatal("meal was acknowledged but is not returned by GET /sync/meals/")
	}
	got, err := Decode[Meal](raw.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != meal.Name || got.Date != meal.Date || got.TypeUID == nil || *got.TypeUID != meal.TypeUID {
		t.Fatalf("read back %+v, wrote %+v", got, meal)
	}
}

func liveFind(t *testing.T, c *Client, col Collection, uid string) (Raw, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rows, err := c.List(ctx, col)
	if err != nil {
		t.Fatalf("read back %s: %v", col, err)
	}
	for _, r := range rows {
		if r.UID == uid {
			return r, true
		}
	}
	return Raw{}, false
}
