# Install on Linux ARM64 (Docker, MCP over HTTP)

The adapter runs as one long-lived container and serves MCP (streamable HTTP) at `http://HOST:8080/mcp`.
Every request must carry a bearer token; each person or agent gets their own. The container runs as uid
65532 with a read-only root filesystem, no capabilities, and one named volume for the cache.

Requirements: Linux on arm64 (aarch64), Docker, outbound HTTPS to `www.paprikaapp.com`.

> The image build and `make up` have not been run by the author of this change (no Docker daemon was
> available). The binary was run directly, serving HTTP with token authentication (see [DEMO.md](DEMO.md)),
> the static `linux/arm64` binary was built, and both base image tags were confirmed to exist.
> If `make image` or `make up` fails, please report the output.

## 1. Build the image

On the host, in a checkout of this repository:

```sh
make image                      # docker build -t paprika-adapter:local .
docker run --rm paprika-adapter:local version
```

Building elsewhere: `docker buildx build --platform linux/arm64 -t paprika-adapter:local --load .`

## 2. Credentials and client tokens

```sh
install -d -m 700 ~/.config/paprika-adapter
install -m 600 paprika.env.example ~/.config/paprika-adapter/paprika.env
make token                                         # prints a new random token; run once per client
$EDITOR ~/.config/paprika-adapter/paprika.env      # PAPRIKA_EMAIL, PAPRIKA_PASSWORD, PAPRIKA_TZ, PAPRIKA_AUTH_TOKENS
```

`PAPRIKA_AUTH_TOKENS` is a comma-separated list of `name:token`, for example
`PAPRIKA_AUTH_TOKENS=peter:3f9c…,anna:a71b…`. The name (letters, digits, `_ . -`) appears in the logs next to
every write; the token must be at least 32 characters. The server refuses to start with no entry.

The file is read by the `docker` CLI and passed as environment; it is never mounted into the container and
never part of the image. Leave `PAPRIKA_ENABLE_WRITES=false` for now.

Where secrets live afterwards:

| What | Where | In git / logs / tool output |
|---|---|---|
| Paprika email, password | `~/.config/paprika-adapter/paprika.env`, mode 600 | never |
| Client tokens | the same file, and each client's own MCP configuration | never; logs show the client name only |
| Paprika bearer token | `token` in the `paprika-data` volume, mode 600 | never |
| Cache | `cache.sqlite` in the same volume; holds your recipes, no credentials | n/a |

## 3. First sync and verification (read-only)

```sh
make sync      # pulls everything once; prints per-collection state. The first run fetches every recipe.
make verify    # probes each Paprika read operation; prints a Markdown report
```

`make sync` must end with every collection `fresh`. Paste the `verify` output into
[PROTOCOL.md](PROTOCOL.md#status-on-your-account) together with your Paprika app version: that is the record
of what has actually been verified for your account. Any `FAILED` row or field drift: see [RUNBOOK.md](RUNBOOK.md).

## 4. Start the service

```sh
make up        # docker run -d --name paprika-adapter --restart unless-stopped -p 127.0.0.1:8080:8080 …
make logs      # "paprika-adapter starting … clients=[peter anna]"
```

Check it from the host (`$TOKEN` is one of the tokens from step 2):

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8080/mcp                  # 401
curl -s -X POST http://127.0.0.1:8080/mcp -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
# … "serverInfo":{"name":"paprika-adapter" …
```

`make restart` after any change to `paprika.env` (the environment is fixed when the container is created).
While it runs, the adapter refreshes its cache every `PAPRIKA_SYNC_INTERVAL`.

To have it come back after a reboot without anyone logging in, the Docker daemon itself must start
at boot: `systemctl --user enable docker` and, once, `sudo loginctl enable-linger $USER`.

## 5. Connect an MCP client

Add to the client's MCP configuration (keep that file mode 600: it now holds a token):

```yaml
mcp_servers:
  paprika:
    url: "http://127.0.0.1:8080/mcp"
    headers:
      Authorization: "Bearer PASTE-THIS-CLIENT'S-TOKEN"
    timeout: 120
    connect_timeout: 60
```

Then reload the client. The tools appear as `mcp_paprika_search_recipes`, `mcp_paprika_get_meal_plan`,
and so on. Ask the agent to call `mcp_paprika_sync_status`: `healthy` should be `true`. The exact syntax
depends on the client.

Any MCP client that speaks streamable HTTP and can send an `Authorization` header works the same way.

## 6. Share it with another user

Give each additional person or agent their own entry in `PAPRIKA_AUTH_TOKENS` (`make token`, edit
`paprika.env`, `make restart`) and hand them the token and the URL.

| Where the other client runs | URL | Notes |
|---|---|---|
| Another account on the same machine | `http://127.0.0.1:8080/mcp` | The port is reachable by every account and process on the machine; the token is the only thing that separates them. |
| Another machine on the home network | `http://<host address>:8080/mcp` | Start with `make up PUBLISH=<host address>:8080` (or `0.0.0.0:8080`). Traffic is plain HTTP: anyone who can observe the network sees the token and the data. Put a TLS reverse proxy in front for anything beyond a network you trust. |

What a token allows: every tool the server exposes, including the write tools when they are enabled. There
are no per-client permissions in v1; a client that must stay read-only can hide the write tools on its side
if it has such a setting, which is a convenience, not a control.

To withdraw access, delete that client's entry and `make restart`. To rotate, replace its token the same way.
Logs never contain a token; a rejected one is logged as `mcp request with an unknown token rejected`.

## 7. Enable writes (second milestone)

Only after step 3 is clean and the read tools have been used for a while:

1. Take a backup: Paprika app → export, and `make export` (see [RUNBOOK.md](RUNBOOK.md#backup-and-export)).
2. Run the integration test against a disposable list ([RUNBOOK.md](RUNBOOK.md#verifying-writes)).
3. Set `PAPRIKA_ENABLE_WRITES=true` in `paprika.env`, and `PAPRIKA_APPROVAL=human` if nothing may be written
   without a person pressing approve in the client that asked. `make restart`, then reload the client.

Three more tools appear: `propose_meal_plan_entry`, `propose_grocery_items`, `commit_proposal`. Each write is
logged with the name of the client that made it (`tool=commit_proposal client=anna outcome=ok`).

## Without Docker

`make linux-arm64` produces a static binary, `bin/paprika-adapter-linux-arm64`. Run it as an ordinary user,
for example from a systemd user unit, with the same variables in its environment:

```sh
set -a; . ~/.config/paprika-adapter/paprika.env; set +a
PAPRIKA_LISTEN=127.0.0.1:8080 ./paprika-adapter serve
```

State then lives in `~/.local/state/paprika-adapter` (override with `PAPRIKA_DATA_DIR`).

## Configuration reference

| Variable | Default | Meaning |
|---|---|---|
| `PAPRIKA_EMAIL`, `PAPRIKA_PASSWORD` | required | or `PAPRIKA_EMAIL_FILE` / `PAPRIKA_PASSWORD_FILE` naming a file |
| `PAPRIKA_AUTH_TOKENS` | required by `serve` | `name:token` entries separated by commas or whitespace; or `PAPRIKA_AUTH_TOKENS_FILE` naming a file with one entry per line |
| `PAPRIKA_LISTEN` | `0.0.0.0:8080` in the image, else `127.0.0.1:8080` | address the HTTP server binds; MCP is served at `/mcp` |
| `PAPRIKA_ENABLE_WRITES` | `false` | register the propose/commit tools |
| `PAPRIKA_APPROVAL` | `auto` | `human` = refuse writes unless the client can show an approval prompt |
| `PAPRIKA_TZ` | process zone (UTC in the container) | decides "today" and the default week |
| `PAPRIKA_MAX_AGE` | `5m` | cached data older than this is re-checked before being served |
| `PAPRIKA_SYNC_INTERVAL` | `15m` | background refresh while serving; `0` disables |
| `PAPRIKA_DATA_DIR` | `/data` in the image, else `~/.local/state/paprika-adapter` | cache and Paprika token |
| `PAPRIKA_BASE_URL` | `https://www.paprikaapp.com` | https only, except loopback |
| `PAPRIKA_LOG_LEVEL` | `info` | `debug` also logs every read tool call with the client's name |

Commands: `serve` (default), `token`, `sync`, `verify`, `export [-offline] DIR|-`, `tools`, `version`.

Make targets for the container: `image`, `up`, `down`, `restart`, `logs`, `token`, `sync`, `verify`, `export`.
`PUBLISH=` (default `127.0.0.1:8080`) sets the host address the port is published on.
