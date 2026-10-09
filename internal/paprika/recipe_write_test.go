package paprika_test

import (
	"context"
	"encoding/json"
	"testing"

	"paprika-adapter/internal/paprika"
)

// The expected hash was produced by an independent implementation of the
// documented algorithm (Python, the same one that reproduced the stored hash of
// 79 of 157 recipes on a real account on 2026-10-09).
func TestRecipeHashMatchesTheReferenceImplementation(t *testing.T) {
	recipe := `{"uid": "AAAAAAAA-0000-4000-8000-000000000001", "name": "Crème brûlée / \"quick\"", "ingredients": "2 cups cream\n½ cup sugar",
		"directions": "Heat <gently> & stir.\tDone.", "description": null, "notes": "", "nutritional_info": null, "servings": "4", "difficulty": "",
		"prep_time": "10 mins", "cook_time": null, "total_time": "", "source": "example.com", "source_url": "https://example.com/a/b",
		"image_url": null, "photo": "P.jpg", "photo_hash": "ABC", "photo_large": null, "scale": null, "hash": "OLD",
		"categories": ["b-2", "A-1", "a-0"], "rating": 4, "in_trash": true, "is_pinned": false, "on_favorites": true,
		"created": "2026-01-02 03:04:05", "photo_url": "http://x/y?Signature=s", "on_grocery_list": false, "cook_minutes": null}`
	const want = "A1497B47470BF18DD72C1C683D38AD314DB9C7A8E8E51E29F603A9FCCAC3BDA7"
	got, err := paprika.RecipeHash(json.RawMessage(recipe))
	if err != nil || got != want {
		t.Fatalf("hash = %s, %v\n want  %s", got, err, want)
	}
	// Neither trash state, the old hash, nor fields outside the hashed set change it.
	var obj map[string]any
	_ = json.Unmarshal([]byte(recipe), &obj)
	obj["in_trash"], obj["hash"], obj["photo_url"], obj["cook_minutes"], obj["deleted"] = false, "ANOTHER", nil, 15, true
	changed, _ := json.Marshal(obj)
	if got, _ := paprika.RecipeHash(changed); got != want {
		t.Fatalf("hash depends on a field it must ignore: %s", got)
	}
	obj["rating"] = 5
	changed, _ = json.Marshal(obj)
	if got, _ := paprika.RecipeHash(changed); got == want {
		t.Fatal("hash did not change with the rating")
	}
}

func TestWriteRecipeSendsTheWholeRecipeBack(t *testing.T) {
	const uid = "014031E3-BC74-4CA8-B345-36EBE9C9794B"
	fake := newFake(t)
	fake.UpdateRecipe(uid, "field_from_a_newer_paprika", map[string]any{"keep": "me"})
	c := newClient(t, fake)
	ctx := context.Background()
	current, err := c.Recipe(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	before := fake.Recipe(uid)
	rating, notes := 2, "Line one.\nLine two / with a slash."
	sent, err := c.WriteRecipe(ctx, current, paprika.RecipeEdit{Rating: &rating, Notes: &notes})
	if err != nil {
		t.Fatal(err)
	}
	if fake.Hits("POST /api/v2/sync/recipe/"+uid+"/") != 1 || fake.Hits("POST /api/v2/sync/recipes/") != 0 {
		t.Fatal("the recipe was not posted once to its own URL")
	}
	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	if want, _ := paprika.RecipeHash(sent); body["hash"] != want || body["hash"] == before["hash"] {
		t.Fatalf("stamped hash %v, want the content hash %s", body["hash"], want)
	}
	if body["deleted"] != false || body["rating"] != float64(2) || body["notes"] != notes {
		t.Fatalf("sent rating=%v notes=%q deleted=%v", body["rating"], body["notes"], body["deleted"])
	}
	for _, readOnly := range []string{"photo_url", "on_grocery_list"} {
		if _, ok := body[readOnly]; ok {
			t.Errorf("%s was sent back", readOnly)
		}
	}
	for field, old := range before {
		switch field {
		case "rating", "notes", "hash", "on_grocery_list":
			continue
		}
		oldJSON, _ := json.Marshal(old)
		newJSON, _ := json.Marshal(body[field])
		if string(oldJSON) != string(newJSON) {
			t.Errorf("%s changed from %s to %s", field, oldJSON, newJSON)
		}
	}
	_, err = c.WriteRecipe(ctx, paprika.Raw{UID: uid, JSON: []byte("[]")}, paprika.RecipeEdit{Rating: &rating})
	wantKind(t, err, paprika.KindInvalid)
}
