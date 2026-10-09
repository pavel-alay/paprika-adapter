//go:build integration

package paprika

// Opt-in tests against a real Paprika account. They are the only way to know
// that the unofficial API still behaves as docs/PROTOCOL.md says.
//
//	PAPRIKA_EMAIL, PAPRIKA_PASSWORD   account to test against
//	PAPRIKA_IT_LIST_UID               a disposable grocery list whose name contains "test";
//	                                  enables the grocery write test
//	PAPRIKA_IT_MEAL_DATE              YYYY-MM-DD, a day you do not plan on; enables the meal write test
//	PAPRIKA_IT_RECIPE_UID             optional disposable recipe to plan in the meal write test; if its
//	                                  name contains "test" it also enables the recipe edit test
//	PAPRIKA_IT_PANTRY                 set to 1 to enable the pantry write test
//
//	go test -tags integration -run Live -v ./internal/paprika/
//
// The write tests create one row, read it back, then soft-delete that same row
// (deleted:true, the way the Paprika apps delete) and confirm it is gone.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

// TestLivePhotos is read-only: it downloads one cover thumbnail and one gallery photo.
func TestLivePhotos(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rows, err := c.List(ctx, Photos)
	if err != nil {
		t.Fatalf("photos: %v", err)
	}
	if d := DetectDrift(string(Photos), rows); !d.Empty() {
		t.Errorf("photos: field drift: new %v, gone %v", d.Unknown, d.Missing)
	}
	t.Logf("gallery photos: %d", len(rows))
	if len(rows) > 0 {
		link, err := c.PhotoLink(ctx, rows[0].UID)
		if err != nil {
			t.Fatalf("photo link: %v", err)
		}
		data, mime, err := c.Download(ctx, link)
		if err != nil {
			t.Fatalf("gallery photo: %v", err)
		}
		t.Logf("gallery photo: %d bytes, %s", len(data), mime)
	}

	refs, err := c.RecipeRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, ref := range refs {
		if i == 40 {
			t.Fatal("none of the first 40 recipes has a photo")
		}
		raw, err := c.Recipe(ctx, ref.UID)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := Decode[Recipe](raw.JSON)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Photo == nil || *rec.Photo == "" {
			continue
		}
		if rec.PhotoURL == nil || rec.PhotoHash == nil {
			t.Fatal("a recipe with a photo has no photo_url or photo_hash")
		}
		data, mime, err := c.Download(ctx, *rec.PhotoURL)
		if err != nil {
			t.Fatalf("thumbnail: %v", err)
		}
		sum := fmt.Sprintf("%X", sha256.Sum256(data))
		t.Logf("thumbnail: %d bytes, %s, matches photo_hash: %v", len(data), mime, strings.EqualFold(sum, *rec.PhotoHash))
		if !strings.EqualFold(sum, *rec.PhotoHash) {
			t.Error("thumbnail bytes do not match the recipe's photo_hash")
		}
		return
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

// TestLiveRecipeEdit overwrites rating, notes, description and directions of a
// disposable recipe, reads them back, then writes the original values again.
func TestLiveRecipeEdit(t *testing.T) {
	c := liveClient(t)
	uid := os.Getenv("PAPRIKA_IT_RECIPE_UID")
	if uid == "" {
		t.Skip("PAPRIKA_IT_RECIPE_UID is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	original, err := c.Recipe(ctx, uid)
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	was, err := Decode[Recipe](original.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(was.Name), "test") {
		t.Skipf("recipe %s is not named as a test recipe; refusing to edit it", uid)
	}
	if computed, _ := RecipeHash(original.JSON); computed != was.Hash {
		t.Logf("the stored hash of this recipe is not the one this adapter computes (another client hashed it)")
	}

	stamp := "[adapter-it] " + time.Now().UTC().Format(time.RFC3339)
	rating := was.Rating%5 + 1
	notes, description, directions := stamp+" notes\nsecond line / with a slash", stamp+" description", stamp+" step one\nstep two"
	if _, err := c.WriteRecipe(ctx, original, RecipeEdit{Rating: &rating, Notes: &notes, Description: &description, Directions: &directions}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		now, err := c.Recipe(ctx, uid)
		if err == nil {
			_, err = c.WriteRecipe(ctx, now, RecipeEdit{Rating: &was.Rating, Notes: &was.Notes, Description: &was.Description, Directions: &was.Directions})
		}
		if err != nil {
			t.Errorf("RESTORE FAILED, put back rating %d and the texts of %q by hand: %v", was.Rating, was.Name, err)
			return
		}
		back, err := c.Recipe(ctx, uid)
		if err != nil {
			t.Errorf("restore could not be read back: %v", err)
			return
		}
		got, _ := Decode[Recipe](back.JSON)
		if got.Rating != was.Rating || got.Notes != was.Notes || got.Description != was.Description || got.Directions != was.Directions {
			t.Errorf("restore did not take; check %q in the app", was.Name)
		}
		t.Logf("restored; hash equals the original: %v", got.Hash == was.Hash)
	})

	edited, err := c.Recipe(ctx, uid)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got, err := Decode[Recipe](edited.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rating != rating || got.Notes != notes || got.Description != description || got.Directions != directions {
		t.Fatalf("read back rating=%d notes=%q description=%q directions=%q", got.Rating, got.Notes, got.Description, got.Directions)
	}
	if got.Hash == was.Hash {
		t.Error("the content hash did not change with the content")
	}
	// Everything that was not edited must have survived the whole-object replace.
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(original.JSON, &before)
	_ = json.Unmarshal(edited.JSON, &after)
	for field, old := range before {
		switch field {
		case "rating", "notes", "description", "directions", "hash", "photo_url":
			continue
		}
		if string(after[field]) != string(old) {
			t.Errorf("%s changed from %s to %s", field, old, after[field])
		}
	}
	if len(after) != len(before) {
		t.Errorf("the recipe had %d fields and now has %d", len(before), len(after))
	}
}

func TestLivePantryWrite(t *testing.T) {
	c := liveClient(t)
	if os.Getenv("PAPRIKA_IT_PANTRY") != "1" {
		t.Skip("PAPRIKA_IT_PANTRY is not 1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	expires := "2099-12-31 00:00:00"
	item := NewPantryItem{UID: NewUID(), Ingredient: "[adapter-it] " + time.Now().UTC().Format(time.RFC3339), Quantity: "1 test",
		InStock: true, HasExpiration: true, ExpirationDate: &expires, PurchaseDate: time.Now().UTC().Format("2006-01-02") + " 00:00:00"}
	if err := c.AddPantryItems(ctx, []NewPantryItem{item}); err != nil {
		t.Fatalf("add: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.upsert(ctx, Pantry, []pantryWire{{NewPantryItem: item, Deleted: true}}); err != nil {
			t.Errorf("CLEANUP FAILED, delete %q from the pantry by hand: %v", item.Ingredient, err)
			return
		}
		if _, found := liveFind(t, c, Pantry, item.UID); found {
			t.Errorf("soft-deleted pantry item %q is still returned; delete it by hand", item.Ingredient)
		}
	})
	raw, found := liveFind(t, c, Pantry, item.UID)
	if !found {
		t.Fatal("item was acknowledged but is not returned by GET /sync/pantry/")
	}
	if d := DetectDrift(string(Pantry), []Raw{raw}); !d.Empty() {
		t.Errorf("pantry field drift: new %v, gone %v", d.Unknown, d.Missing)
	}
	got, err := Decode[PantryItem](raw.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ingredient != item.Ingredient || got.Quantity != item.Quantity || !got.InStock || !got.HasExpiration ||
		got.ExpirationDate == nil || *got.ExpirationDate != expires || got.PurchaseDate == nil || *got.PurchaseDate != item.PurchaseDate {
		t.Fatalf("read back %s, wrote %+v", raw.JSON, item)
	}
	t.Logf("read back: %s", raw.JSON)

	// Posting the same uid again must not create a second row.
	if err := c.AddPantryItems(ctx, []NewPantryItem{item}); err != nil {
		t.Fatalf("repeat add: %v", err)
	}
	rows, err := c.List(ctx, Pantry)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if p, _ := Decode[PantryItem](r.JSON); p.Ingredient == item.Ingredient {
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
