# Paprika sync protocol: what the adapter relies on, and how sure we are

Paprika publishes no API. Everything below is reverse-engineered by other people
and re-checked only as far as stated. Treat it as unstable.

Scope: **Paprika 3** cloud sync only (`/api/v2/sync/*` plus the `/api/v1/account/login/` login).
Paprika 1/2 formats and `.paprikarecipes` files are not supported.

## Verified against a real account

**Reads: verified on 2026-10-09**, first from the cache a full sync
produced, then with the live integration test (`TestLiveReads`, passed). The apps in use were
Paprika 3.8.4. **Writes: the three create-only writes and the meal-plan edit verified on the same day** with the live
integration tests, **the pantry edit and removal on 2026-10-10**. The recipe edit is not verified yet.

| Write | Result on 2026-10-09 |
|---|---|
| Meal-plan entry (`TestLiveMealWrite`) | passed: one meal by name on 2027-12-31 was accepted, returned by `GET /sync/meals/` with the same name, date and meal type, and gone from it after the soft delete |
| Meal-plan edit (`TestLiveMealEdit`) | passed: an entry the test created itself was moved a day, given another meal type and other text by posting its whole row again under the same uid; Paprika returned the changed row, still one row, with `scale`, `is_ingredient` and `order_flag` as before |
| Meal-plan entry pointing at a recipe (`recipe_uid`) | not run; needs `PAPRIKA_IT_RECIPE_UID` |
| Grocery items (`TestLiveGroceryWrite`) | passed against the list `adapter-test`: one item was accepted, returned by `GET /sync/groceries/` with the same name and list, unpurchased; posting the same uid again left exactly one row; gone after the soft delete |
| Pantry items (`TestLivePantryWrite`) | passed: one item with a quantity and an expiration date was accepted and returned by `GET /sync/pantry/` with the same values, in stock; posting the same uid again left exactly one row; gone after the soft delete. The returned row has the 11 expected fields, which also settles the pantry field set below |
| Pantry edit and removal (`TestLivePantryEdit`, 2026-10-10) | passed: an item the test created itself was renamed, given another quantity, marked out of stock and given an expiration and a purchase date by posting its whole row again under the same uid; Paprika returned the changed row with every other field as before. Posting it with `has_expiration: false`, `expiration_date: null` removed the expiration date. Posting the whole row with `deleted: true` made `GET /sync/pantry/` stop returning it |
| Aisle of a new grocery item | sent `aisle: ""`, `aisle_uid: null`; Paprika stored and returned exactly that, it does not assign an aisle itself. Where the apps display such an item has not been looked at |

| Collection | Rows | Result |
|---|---|---|
| Login, change counters, recipe list | | worked: every collection below ended `fresh` with no error |
| Recipes | 157 | 36 fields: the 28 known ones plus 8 new, all present, none missing |
| Categories | 11 | 1 new field (`cookbook_uid`), none missing |
| Meals | 963 | fields as expected |
| Meal types, aisles, grocery ingredients, grocery lists, grocery items | 4, 26, 8, 2, 3 | fields as expected |
| Pantry | 0 | request worked; field set verified later that day through the pantry write test |

New fields found, since added to the adapter's known list:

| Where | Fields | Values seen |
|---|---|---|
| Recipe | `prep_minutes`, `cook_minutes`, `total_minutes`, `servings_min`, `servings_max`, `nutrition`, `cookbook_uid`, `metadata_version` | `null` on all 157 recipes |
| Category | `cookbook_uid` | not inspected |

Because every value is `null`, the types of the new recipe fields are unknown. The adapter stores them and
reports them in exports but does not decode or expose them; the older text fields (`prep_time`, `cook_time`,
`total_time`, `servings`, `nutritional_info`) are still what carries the data (26, 38, 47, 103 and 34 recipes
have them filled). Revisit when a recipe shows a non-null value.

Still to do:

1. Add an item without a known aisle through the tools (or `cmd/demo -live`) and look in the Paprika app at
   where it shows up.
2. The tool path (propose, approve, commit) has run against the fake only; the integration tests exercise the
   client layer underneath it.

## Evidence levels

| Level | Meaning |
|---|---|
| **C** | Wire capture of the Paprika 3.8.4 macOS client (mitmproxy, sanitised HAR) in `bojanrajkovic/mcp-paprika` at `b7de3b8` (captures dated 2026-05) |
| **T** | Described in `aarons22/paprika-tools` `API_REFERENCE.md` at `361b495`; its author reports live checks (meal writes: 2026-06-29) |
| **L** | Observed by this project against `www.paprikaapp.com` on 2026-10-08, without an account |
| **A** | Observed on a real account, see [Verified against a real account](#verified-against-a-real-account) |
| **I** | Inferred from the above; nobody has confirmed it |

Fixture tests in this repo prove the adapter matches the shapes below. They prove nothing about Paprika.

## Operations

| # | Operation | Request | Evidence | On a real account |
|---|---|---|---|---|
| 1 | Login | `POST /api/v1/account/login/`, form fields `email`, `password` → `{"result":{"token"}}` | C (code), T | **A** 2026-10-09 |
| 2 | Bad login | HTTP **200** with `{"error":{"code":0,"message":"Account not found."}}` | L | n/a |
| 3 | Missing/expired token | HTTP 401 with `{"error":{"code":0,"message":"Invalid or expired credentials."}}` | L | n/a |
| 4 | Change counters | `GET /api/v2/sync/status/` → `{"result":{"recipes":952,"groceries":1323,…}}` | C | **A** 2026-10-09 (request works; what the counters mean is still inferred) |
| 5 | Recipe list | `GET /sync/recipes/` → `[{uid, hash}]` | C, T | **A** 2026-10-09 |
| 6 | Recipe detail | `GET /sync/recipe/{uid}/` → 28-field object in the captures, 36 fields now | C, T | **A** 2026-10-09, 157 recipes |
| 7 | Unknown recipe | HTTP 200 with `{"error":{"message":"Recipe not found."}}` | C (code comment) | **A** 2026-10-09: reported as `not_found` |
| 8 | Categories, pantry, meals, meal types, grocery lists, grocery items, aisles, grocery ingredients | `GET /sync/{collection}/` → every row, unpaginated | C, T | **A** 2026-10-09 |
| 9 | Add grocery items | `POST /sync/groceries/`, multipart part `data` = gzip of a JSON **array** of complete objects → `{"result":true}` | C, T | **A** 2026-10-09 |
| 10 | Add meal-plan entry | `POST /sync/meals/`, same envelope | C, T | **A** 2026-10-09, by name; with `recipe_uid` not verified |
| 11 | Add pantry items | `POST /sync/pantry/`, same envelope; the macOS client sends no `notes` and no `location_uid` | C | **A** 2026-10-09 |
| 12 | Malformed new uid | HTTP 200 with `{"error":{"message":"Invalid uid."}}` | C (docs) | not verified |
| 13 | Gallery photo list | `GET /sync/photos/` → `[{uid, recipe_uid, filename, name, order_flag, hash}]`, no link | docs of `mcp-paprika` | **A** 2026-10-09, 53 rows |
| 14 | Gallery photo link | `GET /sync/photo/{uid}/` → `{"result":{"photo_url"}}` | docs of `mcp-paprika` | **A** 2026-10-09 |
| 15 | Photo download | `GET` of a `photo_url`: a signed link to `s3.amazonaws.com/uploads.paprikaapp.com/…`, no API token | docs of `mcp-paprika` | **A** 2026-10-09: JPEG, thumbnail about 35 kB, gallery photo about 240 kB |
| 16 | Overwrite a recipe | `POST /sync/recipe/{uid}/`, multipart part `data` = gzip of **one** JSON object (not an array): the whole recipe, without `photo_url` and `on_grocery_list`, with `deleted` and a recomputed `hash` → `{"result":true}` | C | **not verified** |
| 17 | Edit a meal-plan entry | `POST /sync/meals/` with the whole existing row under its uid, changed fields overwritten | C (whole-object upsert) | **A** 2026-10-09 |
| 18 | Edit a pantry item | `POST /sync/pantry/` with the whole existing row under its uid, changed fields overwritten | C (whole-object upsert) | **A** 2026-10-10 |
| 19 | Remove pantry items | `POST /sync/pantry/` with each whole existing row and `deleted: true` | C (soft delete) | **A** 2026-10-10 |

Both `www.paprikaapp.com` and `paprikaapp.com` answered identically (L). The adapter uses `www`.

## Facts the design depends on

- **Writes are upserts by uid and replace the whole object** (C; A 2026-10-09 for "same uid twice gives one row"). There is no create-vs-update. When creating, the adapter
  therefore only posts uids it minted itself and checks first that Paprika does not already hold them. When
  editing or removing, it posts the row it read from Paprika a moment before, never one from the cache.
- **New uids must be canonical uppercase UUIDs** (C). Server-minted uids come in other shapes (64 hex chars,
  `uuid-number-hex`); the adapter treats every uid as an opaque string and only validates the ones it mints.
- **Errors often arrive as HTTP 200** (L, C). A 200 is success only if the body has `result`.
- **`status` values are change counters, not row counts** (I: the capture shows `groceries: 1323` beside an
  empty grocery list). The adapter skips a download when a counter has not moved. Because this is inferred,
  every collection is re-downloaded at least every 6 hours regardless.
- **Recipe `hash` changes when a recipe changes** (C, T). The adapter re-fetches a recipe only when its hash differs.
- **Soft-deleted rows are not returned by `GET`** (A for meals, grocery items and pantry items, 2026-10-09). The
  removal of pantry items and the integration test's cleanup depend on it.
- **Token lifetime is unknown** (T). On a 401 the adapter logs in again once and retries.
- **Rate limits are unknown** (T). The adapter makes one `status` request per refresh, fetches recipes four at a
  time, backs off after failures, and after Paprika rejects a login waits five minutes before trying another.

## Where the adapter deliberately differs from the Paprika apps

| Topic | Apps | Adapter | Why |
|---|---|---|---|
| Aisle for a new grocery item | always send an aisle | sends the aisle Paprika already remembers for that ingredient, otherwise `aisle: ""`, `aisle_uid: null` | no guessing; `paprika-tools` reports empty aisles work (T). Accepted and stored as sent (A 2026-10-09); where the apps display such an item is still to be looked at |
| Grocery ingredient catalog | post a companion `groceryingredients` row | does not | keeps v1 to a single write per commit |
| Aisle for a new pantry item | send the aisle name and uid | the aisle Paprika remembers for that ingredient, otherwise `aisle: ""`, `aisle_uid: ""` | the captures show stored pantry rows with an empty `aisle_uid`; accepted as sent (A 2026-10-09) |
| Pantry `notes`, `location_uid` | macOS omits them on write | omits them | matches the capture (C); Paprika returns them as `null` (A) |
| Meal `is_ingredient`, `scale` | macOS omits them on write | omits them when creating; sends them back as read when editing, so that the whole-row replace cannot drop a scale | create matches the capture (C); the edit with both fields present was accepted (A 2026-10-09) |
| `notify` ping after a write | used for recipe deletes only | never | not used by the apps for these collections |
| Login | form body; `paprika-tools` also describes Basic auth | form body only | matches the implementation built from captures. If login fails with correct credentials, this is the first thing to revisit |

## Recipe edits (C, not yet verified on a real account)

- The capture of the macOS client shows a recipe saved as a single object posted to the recipe's own URL,
  27 keys: the 28 it reads, minus `photo_url` and `on_grocery_list`, plus `deleted`. A rating change is in
  the capture. The adapter sends the same, plus any further field Paprika returned (the eight found on
  2026-10-09), so that a whole-object replace cannot drop them.
- `hash` is client-owned: the uppercase SHA-256 of a compact JSON array of 27 fields in alphabetical key
  order, as `mcp-paprika` derived it from Paprika's framework (I for this project: reimplemented from that
  description). According to the same notes Paprika does not validate it on an ordinary save; other devices
  use it to notice the change.
- Checked against the cache of a real account on 2026-10-09: the algorithm reproduces the stored hash of 79 of 157
  recipes, among them recipes created in 2025 and 2026. The other 78 (all trashed recipes, all with a `scale`,
  many older ones) carry a hash from some other client or scheme. An edited recipe always gets the hash this
  algorithm gives.
- **To verify**: the live test `TestLiveRecipeEdit` against a throwaway recipe, then a look at that recipe in
  the apps on each platform in use.

## Photos (all A, 2026-10-09)

- A recipe with a picture has `photo` (file name of a small cover thumbnail), `photo_hash` and, in the recipe
  detail, `photo_url`. On the account checked, 126 of 157 recipes have one. The reference notes say `photo_url` is
  empty for uploaded photos; here it was set for every recipe with a `photo`.
- `photo_hash` is the SHA-256 of the thumbnail bytes (checked on three recipes). The `hash` of a gallery photo
  is **not** the SHA-256 of what its link serves (checked on one); the adapter does not rely on either.
- Full-resolution photos are separate rows in `photos`, linked by `recipe_uid`: 53 rows across 23 recipes, up
  to 18 for one recipe. A recipe's `photo_large` is the `filename` of the gallery photo that is its cover (21 recipes).
- Download links carry `AWSAccessKeyId`, `Expires` and `Signature`, and expire about 10 minutes after they are
  issued, so the link stored with a cached recipe is useless and a fresh one is requested per download.
- Paprika issues the links with the `http` scheme. The same link works over `https`, which is what the adapter uses.
- The adapter downloads only from `s3.amazonaws.com/uploads.paprikaapp.com/` or from the API host. A link to
  any other host fails with `protocol_changed` and names the host: that is the sign Paprika moved its photo storage.

## Detecting change

Drift is judged against the list `knownFields` in `internal/paprika/types.go`, which is meant to equal what
Paprika returns today. It started as the field set of the 3.8.4 captures; the first real sync
showed Paprika had added fields since. A new field is not a fault, but `sync_status` reports
`healthy: false` until someone has looked at it and added it to the list
([RUNBOOK.md](RUNBOOK.md#protocol-changes)).

- `sync_status` and every tool result report `protocol_drift` when Paprika returns a field the adapter does not
  know, or stops returning one it expects. Unknown fields are kept in the cache and in exports.
- A response that does not fit at all fails with `protocol_changed` and leaves the cache untouched.
- `paprika-adapter verify` reruns the whole read matrix on demand.
