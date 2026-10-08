package mcpserver

import (
	"context"
	"strings"
	"time"

	"paprika-adapter/internal/ingredients"
	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
)

type SearchRecipesIn struct {
	Query          string `json:"query,omitempty" jsonschema:"words to find in recipe names, ingredients, categories, descriptions, notes and sources; every word must match, as a prefix. Omit to list recipes by name"`
	Category       string `json:"category,omitempty" jsonschema:"only recipes in a category whose name contains these words"`
	Limit          int    `json:"limit,omitempty" jsonschema:"maximum recipes to return, 1 to 50, default 10"`
	IncludeTrashed bool   `json:"include_trashed,omitempty" jsonschema:"include recipes that are in Paprika's trash"`
}

type RecipeSummary struct {
	UID        string   `json:"uid" jsonschema:"stable Paprika recipe uid"`
	Name       string   `json:"name"`
	Categories []string `json:"categories" jsonschema:"category names"`
	Rating     int      `json:"rating" jsonschema:"0 = unrated, otherwise 1 to 5"`
	Favorite   bool     `json:"favorite"`
	Servings   string   `json:"servings,omitempty"`
	PrepTime   string   `json:"prep_time,omitempty"`
	CookTime   string   `json:"cook_time,omitempty"`
	TotalTime  string   `json:"total_time,omitempty"`
	Source     string   `json:"source,omitempty"`
	InTrash    bool     `json:"in_trash,omitempty"`
}

type SearchRecipesOut struct {
	Meta
	Recipes      []RecipeSummary `json:"recipes,omitzero" jsonschema:"best match first"`
	TotalMatches int             `json:"total_matches"`
	Truncated    bool            `json:"truncated" jsonschema:"true when more recipes matched than were returned"`
}

func (s *server) searchRecipes(ctx context.Context, in SearchRecipesIn, out *SearchRecipesOut) *ToolError {
	if terr := s.ensure(ctx, &out.Meta, paprika.Recipes, string(paprika.Categories)); terr != nil {
		return terr
	}
	raws, total, err := s.Store.SearchRecipes(ctx, store.SearchQuery{
		Text: in.Query, Category: in.Category, IncludeTrashed: in.IncludeTrashed, Limit: clamp(in.Limit, 10, 50),
	})
	if err != nil {
		return fromErr(err)
	}
	cats, err := s.categoryNames(ctx)
	if err != nil {
		return fromErr(err)
	}
	out.Recipes = make([]RecipeSummary, 0, len(raws))
	for _, raw := range raws {
		r, err := paprika.Decode[paprika.Recipe](raw)
		if err != nil {
			return fromErr(err)
		}
		sum := RecipeSummary{
			UID: r.UID, Name: r.Name, Categories: []string{}, Rating: r.Rating, Favorite: r.OnFavorites, Servings: r.Servings,
			PrepTime: r.PrepTime, CookTime: r.CookTime, TotalTime: r.TotalTime, Source: r.Source, InTrash: r.InTrash,
		}
		for _, uid := range r.Categories {
			if name, ok := cats[uid]; ok {
				sum.Categories = append(sum.Categories, name)
			}
		}
		out.Recipes = append(out.Recipes, sum)
	}
	out.TotalMatches, out.Truncated = total, total > len(raws)
	return nil
}

type GetRecipeIn struct {
	UID string `json:"uid" jsonschema:"Paprika recipe uid, from search_recipes or a meal-plan entry"`
}

type RecipeDetail struct {
	UID               string             `json:"uid"`
	Name              string             `json:"name"`
	Hash              string             `json:"hash" jsonschema:"Paprika's content hash; it changes whenever the recipe is edited"`
	IngredientsRaw    string             `json:"ingredients_raw" jsonschema:"ingredient text exactly as stored in Paprika, one ingredient per line; this is the authoritative version"`
	IngredientsParsed []ingredients.Line `json:"ingredients_parsed" jsonschema:"best-effort split of ingredients_raw into quantity, unit and item; a convenience that may be wrong"`
	Directions        string             `json:"directions"`
	Description       string             `json:"description,omitempty"`
	Notes             string             `json:"notes,omitempty"`
	NutritionalInfo   string             `json:"nutritional_info,omitempty"`
	Servings          string             `json:"servings,omitempty"`
	Difficulty        string             `json:"difficulty,omitempty"`
	PrepTime          string             `json:"prep_time,omitempty"`
	CookTime          string             `json:"cook_time,omitempty"`
	TotalTime         string             `json:"total_time,omitempty"`
	Source            string             `json:"source,omitempty"`
	SourceURL         string             `json:"source_url,omitempty"`
	Categories        []NamedRef         `json:"categories"`
	Rating            int                `json:"rating" jsonschema:"0 = unrated, otherwise 1 to 5"`
	Favorite          bool               `json:"favorite"`
	InTrash           bool               `json:"in_trash"`
	Created           string             `json:"created,omitempty" jsonschema:"creation time as stored by Paprika, YYYY-MM-DD HH:MM:SS, time zone unspecified"`
	CachedAt          string             `json:"cached_at" jsonschema:"RFC 3339 UTC time this copy was fetched from Paprika"`
}

type GetRecipeOut struct {
	Meta
	Recipe *RecipeDetail `json:"recipe,omitempty"`
}

func (s *server) getRecipe(ctx context.Context, in GetRecipeIn, out *GetRecipeOut) *ToolError {
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		return invalid("uid is required")
	}
	if terr := s.ensure(ctx, &out.Meta, paprika.Recipes, string(paprika.Categories)); terr != nil {
		return terr
	}
	cached, ok, err := s.Store.Recipe(ctx, uid)
	if err != nil {
		return fromErr(err)
	}
	if !ok {
		return notFound("no recipe with uid " + uid + " in the cache")
	}
	r, err := paprika.Decode[paprika.Recipe](cached.Raw)
	if err != nil {
		return fromErr(err)
	}
	cats, err := s.categoryNames(ctx)
	if err != nil {
		return fromErr(err)
	}
	d := &RecipeDetail{
		UID: r.UID, Name: r.Name, Hash: r.Hash, IngredientsRaw: r.Ingredients, IngredientsParsed: ingredients.Parse(r.Ingredients),
		Directions: r.Directions, Description: r.Description, Notes: r.Notes, NutritionalInfo: r.NutritionalInfo,
		Servings: r.Servings, Difficulty: r.Difficulty, PrepTime: r.PrepTime, CookTime: r.CookTime, TotalTime: r.TotalTime,
		Source: r.Source, SourceURL: r.SourceURL, Categories: []NamedRef{}, Rating: r.Rating, Favorite: r.OnFavorites,
		InTrash: r.InTrash, Created: r.Created, CachedAt: cached.FetchedAt.UTC().Format(time.RFC3339),
	}
	for _, c := range r.Categories {
		if name, ok := cats[c]; ok {
			d.Categories = append(d.Categories, NamedRef{UID: c, Name: name})
		}
	}
	out.Recipe = d
	return nil
}

func (s *server) categoryNames(ctx context.Context) (map[string]string, error) {
	cats, err := store.Load[paprika.Category](ctx, s.Store, paprika.Categories)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(cats))
	for _, c := range cats {
		names[c.UID] = c.Name
	}
	return names, nil
}
