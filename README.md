# paprika-adapter

A small, self-hosted bridge between one household's **Paprika 3** cloud sync account and an MCP client.
It caches Paprika data in local SQLite and exposes a handful of typed tools over
stdio. It is not a recipe database and has no web UI: Paprika stays the source of truth.

> **Unofficial API.** Paprika publishes no API; this talks to the same sync endpoints the apps use.
> **Nothing authenticated has been verified against a real account yet** (it was built without credentials).
> Run `verify` and the integration test before relying on it: [docs/PROTOCOL.md](docs/PROTOCOL.md).

## What it does

| | |
|---|---|
| Read tools | `search_recipes`, `get_recipe`, `list_pantry`, `get_meal_plan`, `list_grocery_lists`, `list_grocery_items`, `sync_status` |
| Write tools (off by default) | `propose_meal_plan_entry`, `propose_grocery_items`, `commit_proposal` |
| Not in v1 | delete, edit, move, bulk operations, recipe writes, photos, menus |

- Every result is compact typed JSON with Paprika's own uids and an explicit `sync` block
  (`fresh` / `stale` / `never_synced`, last sync time, last error, protocol drift).
- Sync is incremental: one `status` call per refresh, only changed collections are downloaded, only recipes
  whose hash changed are fetched.
- Recipe ingredient text is stored and returned verbatim (`ingredients_raw`); the parsed form is optional sugar.
- The cache is never pushed back. Writes are create-only, use freshly minted uids, check Paprika first,
  are verified by reading back, and are safe to retry.
- A write needs `confirm: true` from the agent and, where the client supports MCP elicitation,
  an explicit approval from the user.
- Recipe and list text is returned as data. It can come from anywhere (imported web recipes), so the agent
  should not treat it as instructions; the approval step is what stands between such text and a write.
- Credentials come from the environment, the token is cached with mode 600, and neither appears in logs,
  errors, tool results or exports (tested).

## Quick start

```sh
make test                 # fixture-based tests, no network
make demo                 # end-to-end flow against the built-in fake Paprika
make image                # container image
```

Then follow [docs/INSTALL.md](docs/INSTALL.md) (Linux ARM64, Docker, client config).

## Documentation

| | |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | install, credentials, client configuration, settings |
| [docs/TOOLS.md](docs/TOOLS.md), [docs/tools.json](docs/tools.json) | tool reference and exact schemas |
| [docs/PROTOCOL.md](docs/PROTOCOL.md) | what is known about the sync API, with evidence and verification status |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | auth failure, sync conflicts, protocol changes, write verification, backup/export |
| [docs/DEMO.md](docs/DEMO.md) | transcript of the end-to-end flow |

## Layout

```
cmd/paprika-adapter    CLI: serve (stdio MCP), sync, verify, export, tools
cmd/demo               MCP client that walks the end-to-end flow
internal/paprika       Paprika client: login, envelope, reads, the two writes, drift detection
internal/store         SQLite cache: recipes + FTS5 search, other collections, sync state, proposals
internal/syncer        incremental pull, freshness reporting
internal/mcpserver     the tools
internal/ingredients   best-effort ingredient line parser
internal/paprikatest   in-memory fake Paprika with synthetic fixtures (tests and demo)
```

Go 1.27, three direct dependencies: the official MCP Go SDK, `modernc.org/sqlite` (pure Go, so the binary is
static and needs no cgo on ARM64), and `google/uuid`.

## Tests

| Kind | Command | Needs |
|---|---|---|
| Unit and fixture tests (client, cache, sync, every tool, write/retry/approval paths, no-secret-leak checks) | `make test` | nothing |
| End-to-end over stdio against the fake | `make demo` | a loopback port |
| Live protocol check, read-only | `paprika-adapter verify` | credentials |
| Live read + write test with cleanup | `make integration` | credentials and a disposable list, see runbook |

## Credits

Protocol knowledge comes from [bojanrajkovic/mcp-paprika](https://github.com/bojanrajkovic/mcp-paprika)
(wire captures and write-format notes) and [aarons22/paprika-tools](https://github.com/aarons22/paprika-tools)
(API notes). No code or fixture data was copied from either; the fixtures here are synthetic.
