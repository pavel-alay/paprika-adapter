package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg" // sizes of the formats Paprika stores
	_ "image/png"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
	"github.com/pavel-alay/paprika-adapter/internal/store"
)

const (
	sizeThumbnail = "thumbnail"
	sizeFull      = "full"
)

type GetRecipePhotoIn struct {
	UID   string `json:"uid" jsonschema:"recipe uid from search_recipes"`
	Size  string `json:"size,omitempty" jsonschema:"thumbnail (default) = the small cover image, a few tens of kilobytes; full = a full-resolution gallery photo, often several hundred kilobytes, which only some recipes have"`
	Index int    `json:"index,omitempty" jsonschema:"only with size=full: which gallery photo, 1 to get_recipe.gallery_photos, in the order Paprika shows them; default is the cover photo"`
}

// RecipePhoto describes the image returned alongside it as an image content block.
type RecipePhoto struct {
	Recipe        NamedRef `json:"recipe"`
	Size          string   `json:"size" jsonschema:"thumbnail or full"`
	Index         int      `json:"index,omitempty" jsonschema:"position of this photo in the gallery, for size=full"`
	PhotoUID      string   `json:"photo_uid,omitempty" jsonschema:"stable Paprika uid of the gallery photo, for size=full"`
	MIMEType      string   `json:"mime_type"`
	Bytes         int      `json:"bytes"`
	Width         int      `json:"width,omitempty" jsonschema:"pixels"`
	Height        int      `json:"height,omitempty" jsonschema:"pixels"`
	GalleryPhotos int      `json:"gallery_photos" jsonschema:"how many full-resolution photos this recipe has"`
}

type GetRecipePhotoOut struct {
	Meta
	Photo *RecipePhoto `json:"photo,omitempty" jsonschema:"what the image content block of this result shows"`
}

// getRecipePhoto fetches the image from Paprika at call time: the cache holds
// only which photos exist, and download links expire within minutes.
func (s *server) getRecipePhoto(ctx context.Context, in GetRecipePhotoIn, out *GetRecipePhotoOut) *ToolError {
	uid, size := strings.TrimSpace(in.UID), strings.TrimSpace(in.Size)
	if uid == "" {
		return invalid("uid is required")
	}
	if size == "" {
		size = sizeThumbnail
	}
	if size != sizeThumbnail && size != sizeFull {
		return invalid("size must be thumbnail or full")
	}
	if in.Index < 0 || (in.Index > 0 && size != sizeFull) {
		return invalid("index is a position from 1 and needs size=full")
	}
	if terr := s.ensure(ctx, &out.Meta, paprika.Recipes, string(paprika.Photos)); terr != nil {
		return terr
	}
	cached, ok, err := s.Store.Recipe(ctx, uid)
	if err != nil {
		return fromErr(err)
	}
	if !ok {
		return notFound("no recipe with uid " + uid + " in the cache")
	}
	recipe, err := paprika.Decode[paprika.Recipe](cached.Raw)
	if err != nil {
		return fromErr(err)
	}
	gallery, err := s.gallery(ctx, uid)
	if err != nil {
		return fromErr(err)
	}
	photo := &RecipePhoto{Recipe: NamedRef{UID: recipe.UID, Name: recipe.Name}, Size: size, GalleryPhotos: len(gallery)}

	var link string
	switch {
	case size == sizeThumbnail:
		if deref(recipe.Photo) == "" {
			return notFound(fmt.Sprintf("recipe %q has no photo", recipe.Name))
		}
		raw, err := s.Client.Recipe(ctx, uid) // for a link that has not expired
		if err != nil {
			return fromErr(err)
		}
		current, err := paprika.Decode[paprika.Recipe](raw.JSON)
		if err != nil {
			return fromErr(err)
		}
		if link = deref(current.PhotoURL); link == "" {
			return notFound(fmt.Sprintf("recipe %q no longer has a photo", recipe.Name))
		}
	case len(gallery) == 0:
		return notFound(fmt.Sprintf("recipe %q has no full-resolution photo; use size=thumbnail", recipe.Name))
	case in.Index > len(gallery):
		return invalid(fmt.Sprintf("index is %d but recipe %q has %d gallery photo(s)", in.Index, recipe.Name, len(gallery)))
	default:
		photo.Index = in.Index
		if photo.Index == 0 {
			photo.Index = 1
			for i, p := range gallery {
				if p.Filename == deref(recipe.PhotoLarge) {
					photo.Index = i + 1
				}
			}
		}
		photo.PhotoUID = gallery[photo.Index-1].UID
		if link, err = s.Client.PhotoLink(ctx, photo.PhotoUID); err != nil {
			return fromErr(err)
		}
	}
	data, mime, err := s.Client.Download(ctx, link)
	if err != nil {
		return fromErr(err)
	}
	photo.MIMEType, photo.Bytes = mime, len(data)
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		photo.Width, photo.Height = cfg.Width, cfg.Height
	}
	out.Photo = photo
	if c, _ := ctx.Value(callKey{}).(*call); c != nil {
		c.content = []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: mime}}
	}
	return nil
}

// gallery lists a recipe's full-resolution photos in the order Paprika shows them.
func (s *server) gallery(ctx context.Context, recipeUID string) ([]paprika.Photo, error) {
	all, err := store.Load[paprika.Photo](ctx, s.Store, paprika.Photos)
	if err != nil {
		return nil, err
	}
	var out []paprika.Photo
	for _, p := range all {
		if p.RecipeUID == recipeUID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OrderFlag != out[j].OrderFlag {
			return out[i].OrderFlag < out[j].OrderFlag
		}
		return out[i].UID < out[j].UID
	})
	return out, nil
}
