package mcpserver_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/pavel-alay/paprika-adapter/internal/paprikatest"
)

const (
	coverPhoto = "7A1C9E30-4B2D-4F58-9A6C-0D1E2F3A4B5C" // the roast's photo_large; second in gallery order
	firstPhoto = "C4E6A8B0-2D4F-4618-9A3C-5E7F9B1D3F50"
	pancakes   = "pancake"
)

func TestRecipeReportsItsPhotos(t *testing.T) {
	h := newHarness(t, false)
	if r := h.ok("get_recipe", map[string]any{"uid": roastUID}).Recipe; !r.HasPhoto || r.GalleryPhotos != 2 {
		t.Fatalf("roast: has_photo=%v gallery_photos=%d, want true and 2", r.HasPhoto, r.GalleryPhotos)
	}
	if r := h.ok("get_recipe", map[string]any{"uid": soupUID}).Recipe; !r.HasPhoto || r.GalleryPhotos != 0 {
		t.Fatalf("soup: has_photo=%v gallery_photos=%d, want true and 0", r.HasPhoto, r.GalleryPhotos)
	}
	found := h.ok("search_recipes", map[string]any{"query": pancakes})
	if r := h.ok("get_recipe", map[string]any{"uid": found.Recipes[0].UID}).Recipe; r.HasPhoto || r.GalleryPhotos != 0 {
		t.Fatalf("pancakes: has_photo=%v gallery_photos=%d, want false and 0", r.HasPhoto, r.GalleryPhotos)
	}
}

func TestThumbnailIsReturnedAsAnImage(t *testing.T) {
	h := newHarness(t, false)
	h.ok("get_recipe", map[string]any{"uid": soupUID}) // fills the cache, with a link that will be stale
	h.fake.ExpirePhotoLinks()
	before := h.fake.Hits("GET /api/v2/sync/recipe/" + soupUID)

	r := h.ok("get_recipe_photo", map[string]any{"uid": soupUID})
	if len(r.Images) != 1 || !bytes.Equal(r.Images[0], paprikatest.Thumbnail) {
		t.Fatalf("images = %d block(s), want the thumbnail bytes", len(r.Images))
	}
	p := r.Photo
	if p == nil || p.Size != "thumbnail" || p.MIMEType != "image/jpeg" || p.Bytes != len(paprikatest.Thumbnail) || p.Width != 32 || p.Height != 24 ||
		p.GalleryPhotos != 0 || p.Recipe.Name != "Red Lentil Soup" || p.Index != 0 || p.PhotoUID != "" {
		t.Fatalf("photo = %+v", p)
	}
	if !strings.Contains(r.Text, `"mime_type":"image/jpeg"`) {
		t.Fatalf("no text copy of the result next to the image: %q", r.Text)
	}
	if h.fake.Hits("GET /api/v2/sync/recipe/"+soupUID)-before != 1 {
		t.Fatal("the photo was not fetched through a freshly issued link")
	}
	for _, leak := range []string{"Signature", "fixture-signature", "Expires="} {
		if strings.Contains(h.replies.String(), leak) || strings.Contains(h.logs.String(), leak) {
			t.Fatalf("a signed photo link leaked (%s)", leak)
		}
	}
}

func TestFullSizePhotosComeFromTheGallery(t *testing.T) {
	h := newHarness(t, false)
	cover := h.ok("get_recipe_photo", map[string]any{"uid": roastUID, "size": "full"})
	if len(cover.Images) != 1 || !bytes.Equal(cover.Images[0], paprikatest.FullPhoto) {
		t.Fatal("no full-size image returned")
	}
	if p := cover.Photo; p.Size != "full" || p.PhotoUID != coverPhoto || p.Index != 2 || p.GalleryPhotos != 2 || p.Width != 320 || p.Height != 240 {
		t.Fatalf("cover = %+v, want the photo the recipe names as its cover", p)
	}
	if p := h.ok("get_recipe_photo", map[string]any{"uid": roastUID, "size": "full", "index": 1}).Photo; p.PhotoUID != firstPhoto || p.Index != 1 {
		t.Fatalf("index 1 = %+v", p)
	}
	h.fails("get_recipe_photo", map[string]any{"uid": roastUID, "size": "full", "index": 3}, "invalid_input")
	h.fails("get_recipe_photo", map[string]any{"uid": soupUID, "size": "full"}, "not_found") // thumbnail only
}

func TestPhotoRequestsThatCannotBeServed(t *testing.T) {
	h := newHarness(t, false)
	found := h.ok("search_recipes", map[string]any{"query": pancakes})
	if len(found.Recipes) != 1 {
		t.Fatalf("fixture changed: %d pancake recipes", len(found.Recipes))
	}
	noPhoto := found.Recipes[0].UID
	if r := h.fails("get_recipe_photo", map[string]any{"uid": noPhoto}, "not_found"); len(r.Images) != 0 || !strings.Contains(r.Error.Message, "has no photo") {
		t.Fatalf("recipe without a photo: %+v", r.Error)
	}
	h.fails("get_recipe_photo", map[string]any{"uid": "NOPE"}, "not_found")
	h.fails("get_recipe_photo", map[string]any{"uid": " "}, "invalid_input")
	h.fails("get_recipe_photo", map[string]any{"uid": soupUID, "size": "huge"}, "invalid_input")
	h.fails("get_recipe_photo", map[string]any{"uid": soupUID, "index": 1}, "invalid_input")

	h.fake.FailNext("GET /photos/9B2D4F60-1C3E-4A75-8B9D-6E7F8A9B0C2E.jpg", 503)
	if r := h.fails("get_recipe_photo", map[string]any{"uid": soupUID}, "upstream_unavailable"); !r.Error.Retryable {
		t.Fatalf("photo store outage: %+v", r.Error)
	}
	h.fake.SetDown(true)
	h.now = h.now.Add(time.Hour)
	if r := h.fails("get_recipe_photo", map[string]any{"uid": soupUID}, "upstream_unavailable"); !r.Error.Retryable || len(r.Images) != 0 {
		t.Fatalf("Paprika outage: %+v", r.Error)
	}
}
