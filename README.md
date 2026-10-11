# paprika-adapter

A small, self-hosted bridge between one household's **Paprika 3** cloud sync account and MCP clients.
It runs as one container, caches Paprika data in local SQLite and exposes a handful
of typed tools over HTTP, to clients that present a token. It is not a recipe database and has no web UI:
Paprika stays the source of truth.

> **Unofficial API.** Paprika publishes no API; this talks to the same sync endpoints the apps use.
> **It works with Paprika 3.8.4**, checked against a real account in October 2026. Paprika can change the
> API at any time: run `verify` and the integration test before relying on it, and see
> [docs/PROTOCOL.md](docs/PROTOCOL.md) for what has been verified, operation by operation.

## What it does

| | |
|---|---|
| Read tools | `search_recipes`, `get_recipe`, `get_recipe_photo`, `list_pantry`, `get_meal_plan`, `list_grocery_lists`, `list_grocery_items`, `sync_status` |
| Write tools (off by default) | `propose_meal_plan_entry`, `propose_meal_plan_edit`, `propose_grocery_items`, `propose_pantry_items`, `propose_pantry_edit`, `propose_pantry_removal`, `propose_recipe_edit`, `commit_proposal` |
| Not available | deleting anything but pantry items, creating recipes, editing anything but a meal-plan entry, a pantry item or a recipe's rating, notes, description and directions, photo uploads, menus |

- Every result is compact typed JSON with Paprika's own uids and an explicit `sync` block
  (`fresh` / `stale` / `never_synced`, last sync time, last error, protocol drift).
- Sync is incremental: one `status` call per refresh, only changed collections are downloaded, only recipes
  whose hash changed are fetched.
- Recipe ingredient text is stored and returned verbatim (`ingredients_raw`); the parsed form is optional sugar.
- Recipe photos are returned as MCP image content, fetched from Paprika when asked for and not stored: the
  small cover thumbnail by default, a full-resolution gallery photo on request.
- The cache is never pushed back. Writes that add rows use freshly minted uids, check Paprika first, are
  verified by reading back, and are safe to retry.
- Three writes change existing data: a recipe edit overwrites the rating, notes, description or directions of
  a recipe, a meal-plan edit moves an entry to another day or meal type or plans something else in its
  place, and a pantry edit changes an item's quantity, stock status, dates or name. All work on a copy read
  from Paprika at that moment, so every other field is sent back as it was.
- One write removes data: pantry items, named one by one by uid.
- A write needs `confirm: true` from the agent and, where the client supports MCP elicitation,
  an explicit approval from the user. `PAPRIKA_APPROVAL=none` turns the approval off: a token is then enough.
- Recipe and list text is returned as data. It can come from anywhere (imported web recipes), so the agent
  should not treat it as instructions; the approval step is what stands between such text and a write.
- MCP is served at `/mcp` (streamable HTTP) and only to requests with a known bearer token. Each client has
  its own named token; the name is logged with every write. The port is published on loopback by default
  and speaks plain HTTP, so anything beyond one machine or a trusted network needs a TLS proxy in front.
- Paprika credentials come from the environment, Paprika's token is cached with mode 600, and neither they
  nor the client tokens appear in logs, errors, tool results or exports (tested).

## Quick start

```sh
make test                 # fixture-based tests, no network
make demo                 # end-to-end flow over HTTP against the built-in fake Paprika
make image                # container image
make token                # a token for one MCP client
make up                   # start the service on 127.0.0.1:8080
```

Follow [docs/INSTALL.md](docs/INSTALL.md) for the steps in between (Docker, tokens, client configuration).

## Documentation

| | |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | install, credentials, client tokens, sharing, client configuration, settings |
| [docs/TOOLS.md](docs/TOOLS.md), [docs/tools.json](docs/tools.json) | tool reference and exact schemas |
| [docs/PROTOCOL.md](docs/PROTOCOL.md) | what is known about the sync API, with evidence and verification status |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | client cannot connect, auth failure, sync conflicts, protocol changes, write verification, backup/export |
| [docs/DEMO.md](docs/DEMO.md) | transcript of the end-to-end flow |

## Layout

```
cmd/paprika-adapter    CLI: serve (MCP over HTTP, token auth), token, sync, verify, export, tools
cmd/demo               MCP client that walks the end-to-end flow
internal/paprika       Paprika client: login, envelope, reads, the three create-only writes, the recipe edit and its content hash, drift detection
internal/store         SQLite cache: recipes + FTS5 search, other collections, sync state, proposals
internal/syncer        incremental pull, freshness reporting
internal/mcpserver     the tools
internal/ingredients   best-effort ingredient line parser
internal/paprikatest   in-memory fake Paprika with synthetic fixtures (tests and demo)
```

Go 1.27, three direct dependencies: the official MCP Go SDK, `modernc.org/sqlite` (pure Go, so the binary is
static and needs no cgo), and `google/uuid`.

## Tests

| Kind | Command | Needs |
|---|---|---|
| Unit and fixture tests (client, cache, sync, every tool, write/retry/approval paths, HTTP token auth, no-secret-leak checks) | `make test` | nothing |
| End-to-end over HTTP against the fake | `make demo` | two loopback ports |
| Live protocol check, read-only | `paprika-adapter verify` | credentials |
| Live read + write test with cleanup | `make integration` | credentials and a disposable list, see runbook |

## Credits

Protocol knowledge comes from [bojanrajkovic/mcp-paprika](https://github.com/bojanrajkovic/mcp-paprika)
(wire captures and write-format notes) and [aarons22/paprika-tools](https://github.com/aarons22/paprika-tools)
(API notes). No code or fixture data was copied from either; the fixtures here are synthetic.

## License

[MIT](LICENSE)
