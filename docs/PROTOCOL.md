# Paprika sync protocol: what the adapter relies on, and how sure we are

Paprika publishes no API. Everything below is reverse-engineered by other people
and re-checked only as far as stated. Treat it as unstable.

Scope: **Paprika 3** cloud sync only (`/api/v2/sync/*` plus the `/api/v1/account/login/` login).
Paprika 1/2 formats and `.paprikarecipes` files are not supported.

## Status on your account

**No authenticated operation has been verified against your Paprika account or app version yet.**
This build was developed without credentials. Before trusting it:

1. `paprika-adapter verify` checks every read operation and reports field drift. Paste the output below.
2. The opt-in integration test (see [RUNBOOK.md](RUNBOOK.md#verifying-writes)) checks both writes against a disposable list and date.

```
(paste the output of `paprika-adapter verify` here, with the date and your Paprika app version)
```

## Evidence levels

| Level | Meaning |
|---|---|
| **C** | Wire capture of the Paprika 3.8.4 macOS client (mitmproxy, sanitised HAR) in `bojanrajkovic/mcp-paprika` at `b7de3b8` (captures dated 2026-05) |
| **T** | Described in `aarons22/paprika-tools` `API_REFERENCE.md` at `361b495`; its author reports live checks (meal writes: 2026-06-29) |
| **L** | Observed by this project against `www.paprikaapp.com` on 2026-10-08, without an account |
| **I** | Inferred from the above; nobody has confirmed it |

Fixture tests in this repo prove the adapter matches the shapes below. They prove nothing about Paprika.

## Operations

| # | Operation | Request | Evidence | On your account |
|---|---|---|---|---|
| 1 | Login | `POST /api/v1/account/login/`, form fields `email`, `password` → `{"result":{"token"}}` | C (code), T | not verified |
| 2 | Bad login | HTTP **200** with `{"error":{"code":0,"message":"Account not found."}}` | L | n/a |
| 3 | Missing/expired token | HTTP 401 with `{"error":{"code":0,"message":"Invalid or expired credentials."}}` | L | n/a |
| 4 | Change counters | `GET /api/v2/sync/status/` → `{"result":{"recipes":952,"groceries":1323,…}}` | C | not verified |
| 5 | Recipe list | `GET /sync/recipes/` → `[{uid, hash}]` | C, T | not verified |
| 6 | Recipe detail | `GET /sync/recipe/{uid}/` → 28-field object | C, T | not verified |
| 7 | Unknown recipe | HTTP 200 with `{"error":{"message":"Recipe not found."}}` | C (code comment) | not verified |
| 8 | Categories, pantry, meals, meal types, grocery lists, grocery items, aisles, grocery ingredients | `GET /sync/{collection}/` → every row, unpaginated | C, T | not verified |
| 9 | Add grocery items | `POST /sync/groceries/`, multipart part `data` = gzip of a JSON **array** of complete objects → `{"result":true}` | C, T | not verified |
| 10 | Add meal-plan entry | `POST /sync/meals/`, same envelope | C, T | not verified |
| 11 | Malformed new uid | HTTP 200 with `{"error":{"message":"Invalid uid."}}` | C (docs) | not verified |

Both `www.paprikaapp.com` and `paprikaapp.com` answered identically (L). The adapter uses `www`.

## Facts the design depends on

- **Writes are upserts by uid and replace the whole object** (C). There is no create-vs-update. The adapter
  therefore only ever posts uids it minted itself, checks first that Paprika does not already hold them, and
  never posts a row it read from the cache.
- **New uids must be canonical uppercase UUIDs** (C). Server-minted uids come in other shapes (64 hex chars,
  `uuid-number-hex`); the adapter treats every uid as an opaque string and only validates the ones it mints.
- **Errors often arrive as HTTP 200** (L, C). A 200 is success only if the body has `result`.
- **`status` values are change counters, not row counts** (I: the capture shows `groceries: 1323` beside an
  empty grocery list). The adapter skips a download when a counter has not moved. Because this is inferred,
  every collection is re-downloaded at least every 6 hours regardless.
- **Recipe `hash` changes when a recipe changes** (C, T). The adapter re-fetches a recipe only when its hash differs.
- **Soft-deleted rows are not returned by `GET`** (I). Only the integration test's cleanup depends on it.
- **Token lifetime is unknown** (T). On a 401 the adapter logs in again once and retries.
- **Rate limits are unknown** (T). The adapter makes one `status` request per refresh, fetches recipes four at a
  time, backs off after failures, and after Paprika rejects a login waits five minutes before trying another.

## Where the adapter deliberately differs from the Paprika apps

| Topic | Apps | Adapter | Why |
|---|---|---|---|
| Aisle for a new grocery item | always send an aisle | sends the aisle Paprika already remembers for that ingredient, otherwise `aisle: ""`, `aisle_uid: null` | no guessing; `paprika-tools` reports empty aisles work (T). **Unverified on your account**: check where such an item shows up in the app |
| Grocery ingredient catalog | post a companion `groceryingredients` row | does not | keeps v1 to a single write per commit |
| Meal `is_ingredient`, `scale` | macOS omits them on write | omits them | matches the capture (C) |
| `notify` ping after a write | used for recipe deletes only | never | not used by the apps for these collections |
| Login | form body; `paprika-tools` also describes Basic auth | form body only | matches the implementation built from captures. If login fails with correct credentials, this is the first thing to revisit |

## Detecting change

- `sync_status` and every tool result report `protocol_drift` when Paprika returns a field the adapter does not
  know, or stops returning one it expects. Unknown fields are kept in the cache and in exports.
- A response that does not fit at all fails with `protocol_changed` and leaves the cache untouched.
- `paprika-adapter verify` reruns the whole read matrix on demand.
