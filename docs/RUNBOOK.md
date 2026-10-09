# Runbook

First step for any problem: ask the agent for `sync_status` (add `refresh: true` to test the connection now),
or run `make sync`. Each collection reports `state`, `error.code` and `protocol_drift`.

The adapter only reads from Paprika, except for the three create-only writes (meal-plan entry, grocery
items, pantry items) and the two edits: a recipe's rating, notes, description and directions, and one
meal-plan entry at a time (day, meal type, what is planned). The cache is disposable:
deleting the `paprika-data` volume loses nothing that is not in Paprika.

Restarting the adapter is `make restart`. It is needed after every change to `paprika.env`. Clients lose
their MCP session when it restarts; if one does not reconnect by itself, reload it.

## A client cannot connect

| Symptom | Cause | Fix |
|---|---|---|
| Container exits at once, log says `PAPRIKA_AUTH_TOKENS must hold at least one name:token entry` | no client token configured | `make token`, add `name:token` to `paprika.env`, `make up` |
| Log says `PAPRIKA_AUTH_TOKENS: entry N must look like name:token` or `shorter than 32 characters` | malformed entry | fix the entry; the message never repeats the token |
| HTTP 401 `no bearer token` | the client sends no `Authorization: Bearer …` header | add the header to the client's MCP configuration |
| HTTP 401 `invalid token`, log line `mcp request with an unknown token rejected` | wrong token, or the entry was removed or changed | compare with `paprika.env`; `make restart` if the file was edited after the last start |
| HTTP 403 `session user mismatch` | a session opened with one token is used with another | reconnect the client |
| HTTP 404 `session not found` | the adapter was restarted | reconnect the client |
| HTTP 404 on any other path | MCP is served at `/mcp` only | fix the URL |
| Connection refused from another machine | the port is published on loopback | `make restart PUBLISH=<host address>:8080`, see [INSTALL.md](INSTALL.md#6-share-it-with-another-user) |

A token that may have leaked: replace it in `paprika.env` and `make restart`. The Paprika password is not
affected by a leaked client token, but that client could read everything and, with writes enabled, add
meals and grocery items until the token is replaced.

## Auth failure

This is about the adapter's own login to Paprika, not about MCP clients. Symptom: `error.code: auth_failed`; reads keep answering from cache with `state: stale`; writes refuse.

| Message | Cause | Fix |
|---|---|---|
| `Account not found.` / `Invalid password.` or similar | wrong email or password in `paprika.env` | correct the file, then `make restart` |
| `token refused again right after a successful login` | Paprika accepts the login but not the token | protocol change, see below |
| `login response carries no token` (`protocol_changed`) | login reply changed shape | protocol change, see below |

Notes:

- An expired token is not a failure: the adapter logs in again once, silently.
- After a rejected login the adapter waits 5 minutes before trying again, to avoid hammering the account.
  Restarting it (`make restart`) clears the wait.
- To force a clean login: `make down`, `docker run --rm -v paprika-data:/data busybox rm -f /data/token`,
  `make up`; or reset the whole cache (see [Reset](#reset)).
- Password changed in the Paprika app: update `paprika.env`; nothing else stores it.
- Check the credentials independently by signing in to Paprika's cloud sync in the app itself.

## Sync conflicts

Paprika is the source of truth. The adapter never pushes cached rows, never updates and never deletes, so
it cannot overwrite an edit made in an app. What can still happen:

| Symptom | Meaning | Action |
|---|---|---|
| Data in the client is older than the app | the app has not synced to the cloud yet, or the cache is `stale` | sync the app; `sync_status` with `refresh: true` |
| Something added by the client is missing in the app | the app has not pulled yet | sync the app; confirm with `list_grocery_items` / `get_meal_plan`, which read Paprika's cloud copy |
| `duplicate` on propose or commit | the item or meal is already there (possibly added from a phone meanwhile) | nothing; or repeat with `allow_duplicates` if intended |
| `skipped` entries in a commit result | those items appeared in Paprika between propose and commit | nothing |
| `upstream_unavailable` on commit, "unknown whether Paprika received the write" | the response was lost | call `commit_proposal` again; it re-reads first and cannot double-add |
| `write_not_verified` | Paprika acknowledged but does not return the row yet | call `commit_proposal` again; if it persists, check the app, then treat as a protocol change |
| `uid_conflict` | Paprika holds a different row under the freshly minted uid (should not happen) | nothing was overwritten; propose again and report it |
| `proposal_expired` | more than 15 minutes between propose and commit | propose again |
| Recipe edited in the app, the client shows the old text | recipe hash not re-checked yet | wait `PAPRIKA_MAX_AGE` or refresh; the cached copy is replaced, never merged |
| Recipes stuck `stale` with "N of M changed recipes could not be fetched" | some recipe fetches failed | it resumes on the next sync; persistent failure on the same recipes points at a protocol change |

To undo a write, delete the item or meal in the Paprika app. There is no delete tool on purpose.

To undo a meal-plan edit, propose the reverse: the commit result gives the entry as it was in `meal_before`.

To undo a recipe edit, put the old value back: the result of the commit lists `from` for every changed field,
and so does the proposal. Either edit the recipe in the app or propose the reverse edit. Old values are not
kept anywhere else, so save that result if the text matters.

A recipe edited through the adapter that looks wrong in an app (missing fields, endless re-syncing) points at
the recipe write or its hash no longer matching what Paprika expects: set `PAPRIKA_ENABLE_WRITES=false`,
`make restart`, and treat it as a protocol change (below).

## Protocol changes

Paprika can change its private API at any time. Signs, in rising order of severity:

1. `protocol_drift` in `sync_status`: a field was added (`unknown_fields`) or disappeared (`missing_fields`).
   Reads keep working; unknown fields are preserved in the cache and exports.
2. `error.code: protocol_changed`: a response no longer parses. The affected collection stays `stale`.
3. `rejected` on a write that used to work, or `write_not_verified` that does not clear.

A photo that fails with `protocol_changed: photo link points at an unexpected host: <host>` means Paprika
moved its photo storage. Nothing else is affected. Check the host is Paprika's, then add it next to
`photoStoreHost` in `internal/paprika/photo.go`; do not make the check accept any host.

What to do:

1. **Set `PAPRIKA_ENABLE_WRITES=false`** and `make restart`. Reads from cache are harmless; writes against a
   changed protocol are not.
2. `make verify`. It shows which operation broke and which fields moved. Keep the output.
3. Compare with [PROTOCOL.md](PROTOCOL.md) and the upstream projects it cites (they usually notice first).
4. Fix in `internal/paprika` (wire types in `types.go`, known fields in `knownFields`, envelope in
   `client.go`, write shapes in `write.go`), update `internal/paprikatest` to match, `make test`.
5. Re-run `make verify` and the integration test before turning writes back on. Update PROTOCOL.md.

A missing field the tools read (for example `list_uid` on grocery items) makes results silently emptier
rather than failing. Treat any `missing_fields` as a reason to do steps 1 and 2.

## Verifying writes

Opt-in test against the real account. It creates one grocery item, one meal and one pantry item, reads each
back, then soft-deletes exactly those rows the way the Paprika apps do, and checks they are gone. With a meal
date set it also creates a second entry of its own, edits it, and removes it.

1. In the Paprika app create a grocery list whose name contains `test` (for example `adapter-test`), and
   a throwaway recipe whose name contains `test` (it enables the recipe edit test, which changes its rating
   and texts and then restores them). Sync the app.
2. Find the uids: `list_grocery_lists` in the client, or `make sync` then `search_recipes`.
3. Run on a machine with Go:

```sh
set -a; . ~/.config/paprika-adapter/paprika.env; set +a   # or export PAPRIKA_EMAIL / PAPRIKA_PASSWORD by hand
PAPRIKA_IT_LIST_UID=<uid of the test list> \
PAPRIKA_IT_MEAL_DATE=2027-12-31 \
PAPRIKA_IT_RECIPE_UID=<uid of the throwaway recipe> \
PAPRIKA_IT_PANTRY=1 \
make integration
```

The grocery test refuses to run unless the list's name contains `test`. Without the `PAPRIKA_IT_*` variables
only the read test runs. If cleanup fails the test prints what to delete by hand.
Record the result and date in PROTOCOL.md.

## Backup and export

Do both before enabling writes, and occasionally after.

1. **Authoritative backup: from Paprika itself.** In the app use its Export function
   and choose the *Paprika Recipe Format* (on macOS it is under File → Export). Only this can be re-imported into Paprika.
2. **Adapter export**: every cached row exactly as the sync API returned it, plus a SQLite snapshot.

```sh
make export                                   # writes ./paprika-export-<UTC time>.tar
tar -tf paprika-export-*.tar
#   paprika-export-<time>/manifest.json       when, adapter version, freshness of each collection
#   paprika-export-<time>/recipes.json        full recipe objects
#   paprika-export-<time>/{categories,pantry,meals,mealtypes,grocerylists,groceries,groceryaisles,groceryingredients}.json
#   paprika-export-<time>/cache.sqlite        consistent snapshot of the cache
```

The export refreshes from Paprika first and exits non-zero if any collection is not `fresh` (the files are
still written; `manifest.json` says which). `export -offline` skips the refresh. With the plain binary,
`paprika-adapter export DIR` writes a directory instead of a tar stream.

The export contains your recipes and lists but no password and no token. It is a copy for safekeeping and
inspection, not something Paprika can import, and the adapter never restores from it.

## Reset

```sh
make down && docker volume rm paprika-data && make sync && make up    # empty cache, fresh login, full re-download
```
