# MCP tools

Exact JSON Schemas (input and output, as served by `tools/list`) are in [tools.json](tools.json).
A test fails if that file drifts from the code; regenerate with `make tools`.

The tools are served over streamable HTTP at `/mcp` and need an `Authorization: Bearer <token>` header
([INSTALL.md](INSTALL.md#2-credentials-and-client-tokens)); without one the answer is HTTP 401, before any MCP exchange.
The client exposes them as `mcp_paprika_<tool>` when the server is named `paprika`.

## Common result shape

Every tool returns structured JSON with the same head:

```json
{
  "ok": true,
  "sync": [
    {"collection": "groceries", "state": "fresh", "last_synced_at": "2026-10-08T16:58:48Z", "age_seconds": 0, "rows": 6}
  ]
}
```

| Field | Meaning |
|---|---|
| `ok` | `false` means the call failed; `error` says why and MCP `isError` is set |
| `error.code` | stable code, see below |
| `error.retryable` | `true` if repeating the same call later may work without anyone changing anything |
| `sync[].state` | `fresh`: confirmed against Paprika within `PAPRIKA_MAX_AGE`. `stale`: cached copy, could not be confirmed. `never_synced`: no complete copy cached yet (a first recipe sync may be part-way through; `rows` shows how far). Tools refuse to answer from it |
| `sync[].error` | why the last sync attempt failed (`code`, `message`, `at`) |
| `sync[].protocol_drift` | fields Paprika added (`unknown_fields`) or dropped (`missing_fields`) |

A read with `ok: true` and `state: stale` is real data that may be out of date. The agent should say so.

Conventions: uids are Paprika's own and stable. Adapter timestamps are RFC 3339 UTC. Calendar dates are
`YYYY-MM-DD`. A recipe's `created` is passed through as Paprika stores it (`YYYY-MM-DD HH:MM:SS`, zone unspecified).

### Error codes

| Code | Meaning | Retry |
|---|---|---|
| `invalid_input` | argument missing or malformed; nothing was sent | no |
| `not_found` | no such recipe, list, meal type or proposal | no |
| `auth_failed` | Paprika rejected the credentials | no, see runbook |
| `upstream_unavailable` | network error, timeout, 429 or 5xx; or the first sync has not finished (it continues in the background and on each call) | yes |
| `protocol_changed` | Paprika's response no longer has the expected shape | no, see runbook |
| `rejected` | Paprika refused the request | no |
| `duplicate` | the write would duplicate something already in Paprika | no |
| `confirmation_required` | `commit_proposal` called without `confirm: true` | after approval |
| `approval_declined` | the user declined the approval prompt | no |
| `approval_unavailable` | approval is mandatory and the client cannot prompt, or the prompt failed | no |
| `proposal_expired` | proposals live 15 minutes | propose again |
| `write_not_verified` | written but not readable back yet | yes, repeat the commit |
| `uid_conflict` | Paprika already holds a different row under the minted uid; nothing overwritten | propose again |
| `cache_error` | local SQLite problem | no |

## Read tools (always available)

| Tool | Arguments | Returns |
|---|---|---|
| `search_recipes` | `query?` (all words must match, prefix match, over name, ingredients, categories, description, notes, source), `category?`, `limit?` (1-50, default 10), `include_trashed?` | `recipes[]` (`uid`, `name`, `categories`, `rating`, `favorite`, times, `source`), `total_matches`, `truncated` |
| `get_recipe` | `uid` | `recipe` with `ingredients_raw` (verbatim from Paprika, authoritative), `ingredients_parsed[]` (best effort: `raw`, `quantity`, `unit`, `item`, `note`, `heading`), `directions`, `categories[]`, `hash`, `cached_at` |
| `get_recipe_photo` | `uid`, `size?` (`thumbnail` default, or `full`), `index?` (with `full`: 1 to `gallery_photos`; default the cover) | an **image content block** with the photo, and `photo` describing it (`recipe`, `size`, `index`, `photo_uid`, `mime_type`, `bytes`, `width`, `height`, `gallery_photos`) |
| `list_pantry` | `query?`, `in_stock_only?`, `limit?` (1-500, default 100) | `items[]` (`uid`, `ingredient`, `quantity`, `aisle`, `in_stock`, `purchase_date`, `expiration_date`), `total`, `truncated` |
| `get_meal_plan` | `from?`, `to?` (default: Monday to Sunday of the current week in `PAPRIKA_TZ`; max 92 days) | `from`, `to`, `today`, `timezone`, `meals[]` (`uid`, `date`, `meal_type`, `meal_type_uid`, `name`, `recipe_uid`), `meal_types[]` |
| `list_grocery_lists` | none | `lists[]` (`uid`, `name`, `is_default`, `items`, `unpurchased`) |
| `list_grocery_items` | `list_uid`, `include_purchased?`, `limit?` (1-500, default 200) | `list`, `items[]` (`uid`, `name`, `ingredient`, `quantity`, `instruction`, `aisle`, `purchased`, `recipe`, `recipe_uid`), `total`, `truncated` |
| `sync_status` | `refresh?` (`true` contacts Paprika now) | `healthy`, `writes_enabled`, `paprika_host`, `adapter_version`, and `sync[]` for all ten collections |

### Photos

`get_recipe` reports `has_photo` and `gallery_photos`. A recipe can have a cover thumbnail (small, a few tens
of kilobytes; most recipes with a picture have only this) and a gallery of full-resolution photos (several
hundred kilobytes each; step-by-step pictures live here).

- The image arrives as MCP image content next to a text copy of the JSON result. Whether the model sees it, or
  the user is shown it, depends on the MCP client; a client that ignores image content gets only the description.
- Photos are not cached. Each call asks Paprika for a download link, which is valid for about ten minutes, and
  fetches the image, so the tool needs Paprika to be reachable: otherwise `upstream_unavailable`.
- `not_found` means the recipe has no photo of that kind; `size: full` on a recipe with only a thumbnail says so.
- A full-resolution photo is a large tool result. Ask for `thumbnail` unless the detail matters.
- Download links are never returned or logged. Photos larger than 8 MiB are refused (`rejected`).

`ingredients_parsed` never replaces `ingredients_raw`: joining every `raw` with newlines gives back the stored
text minus blank lines.

## Write tools (only with `PAPRIKA_ENABLE_WRITES=true`)

There are exactly five: four that prepare a write and one that performs it. Three of the writes only create
new rows. One, `propose_recipe_edit`, changes existing data: four fields of a recipe. No delete, move, clear
or bulk tool exists.

| Tool | Arguments | Effect |
|---|---|---|
| `propose_meal_plan_entry` | `date`, `meal_type_uid` (from `get_meal_plan.meal_types`), and exactly one of `recipe_uid` or `name`; `allow_duplicate?` | Writes nothing. Returns `proposal` (`proposal_id`, `summary`, `expires_at`, `meal`) |
| `propose_grocery_items` | `list_uid` (no default list), `items[]` 1-25 of `{name, quantity?, instruction?}`; `allow_duplicates?` | Writes nothing. Returns `proposal` (`proposal_id`, `summary`, `items[]`, `skipped[]`) |
| `propose_pantry_items` | `items[]` 1-25 of `{ingredient, quantity?, expiration_date?}`; `allow_duplicates?` | Writes nothing. Returns `proposal` (`proposal_id`, `summary`, `pantry_items[]`, `skipped[]`). Items are created in stock with today's date (in `PAPRIKA_TZ`) as purchase date |
| `propose_recipe_edit` | `uid`, and at least one of `rating` (0-5, 0 = unrated), `notes`, `append_note`, `description`, `directions` | Writes nothing. Returns `proposal` (`proposal_id`, `summary`, `recipe_edit.changes[]` with `field`, `from`, `to`) |
| `commit_proposal` | `proposal_id`, `confirm: true` | Writes the proposal, reads it back, returns `status`, `verified`, and the rows as read back |

### How a write goes

1. **Propose.** Inputs are validated against data confirmed fresh just now (a stale cache refuses to propose).
   Uids are minted here and stored with the proposal in SQLite. Grocery items already on the list, unpurchased,
   under the same name are put in `skipped` instead of being proposed; so are pantry items whose ingredient
   is already in the pantry, in stock or not (restocking an existing item is an edit, which v1 does not do).
2. **Approve.** The agent shows `summary` to the user. If the client supports MCP elicitation,
   `commit_proposal` also puts the same summary to the user directly and writes only on *accept*.
   With `PAPRIKA_APPROVAL=human` a client that cannot prompt cannot write at all.
3. **Commit.** The adapter re-reads the collection from Paprika, then:
   - rows whose uid is already there are not sent again;
   - rows that would now duplicate something added elsewhere are skipped (`duplicate` if nothing is left);
   - the rest go in one POST, which is never retried automatically.
4. **Read back.** The collection is fetched again and every row is checked by uid. Only then is the result
   `verified: true` and the proposal marked committed.

### Editing a recipe

`propose_recipe_edit` is the only tool that overwrites. What that means:

- `notes`, `description` and `directions` **replace** the field. `append_note` instead adds a paragraph after
  the existing notes, and is the right choice for "add a note". An empty `notes` or `description` clears it;
  `directions` cannot be emptied.
- The proposal lists every change with the old and the new value. That is what the user should see before
  approving, and the commit result repeats it, so the old text can be put back with another edit.
- At commit the recipe is read from Paprika, the edited fields are overwritten on that copy, and the whole
  recipe is written back with a new content hash, then read back. Fields the adapter does not know are
  carried over untouched.
- An edit made in an app to the **same field** between propose and commit is overwritten without warning; this
  was chosen deliberately. Edits to other fields survive. An edit made in an app during the fraction of a
  second between the adapter's read and its write would be lost whatever the field.
- Recipes in the trash cannot be edited. Proposing values the recipe already has answers `duplicate`.
- Name, ingredients, times, servings, categories, photos and everything else cannot be changed.

### Retrying

`commit_proposal` is safe to repeat in every state:

| Situation | What a repeat does |
|---|---|
| Already committed | returns the stored result with `status: already_committed`, no request to Paprika |
| POST failed before reaching Paprika | sends the same uids again |
| POST reached Paprika but the response was lost | finds the uids on re-read, sends nothing, verifies, reports `committed` |
| Same request proposed twice | the second commit finds the items and fails with `duplicate` |
| Recipe edit whose response was lost | finds the fields already changed, writes nothing, verifies; a note is not appended twice |

### Example

```json
// propose_grocery_items {"list_uid": "9E12…F6F6", "items": [{"name": "chicken thighs", "quantity": "1 lb"}]}
{
  "ok": true,
  "proposal": {
    "proposal_id": "3DABFD39-6697-4816-A9E3-6E2FF4AA4924",
    "kind": "grocery_items",
    "expires_at": "2026-10-08T17:13:48Z",
    "summary": "Add 1 item(s) to the Paprika grocery list \"My Grocery List\": 1 lb chicken thighs.",
    "list": {"uid": "9E12…F6F6", "name": "My Grocery List"},
    "items": [{"uid": "81ECA948-E0ED-4A76-980E-1FE11160E08E", "name": "1 lb chicken thighs", "quantity": "1 lb", "aisle": "Meat"}]
  }
}

// commit_proposal {"proposal_id": "3DABFD39-6697-4816-A9E3-6E2FF4AA4924", "confirm": true}
{
  "ok": true,
  "proposal_id": "3DABFD39-6697-4816-A9E3-6E2FF4AA4924",
  "kind": "grocery_items",
  "status": "committed",
  "verified": true,
  "committed_at": "2026-10-08T16:58:48Z",
  "list": {"uid": "9E12…F6F6", "name": "My Grocery List"},
  "items": [{"uid": "81ECA948-E0ED-4A76-980E-1FE11160E08E", "name": "1 lb chicken thighs", "ingredient": "chicken thighs", "quantity": "1 lb", "aisle": "Meat", "purchased": false}],
  "sync": [{"collection": "groceries", "state": "fresh", "last_synced_at": "2026-10-08T16:58:48Z", "age_seconds": 0, "rows": 6}]
}
```

(Captured from the demo run against the fake backend; uids shortened by hand.)
