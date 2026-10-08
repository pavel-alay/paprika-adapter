# Install on Linux ARM64 (Docker, stdio MCP)

The client starts the adapter as a child process and talks MCP over its stdin/stdout. There is no listening port
and no daemon to keep running. The container runs as uid 65532 with a read-only root filesystem, no
capabilities, and one named volume for the cache.

Requirements: Linux on arm64 (aarch64), Docker, outbound HTTPS to `www.paprikaapp.com`.

> The image build has not been run by the author of this change (no Docker daemon was available). The static
> `linux/arm64` binary was built and inspected, and both base image tags were confirmed to exist.
> If `make image` fails, please report the output.

## 1. Build the image

On the host, in a checkout of this repository:

```sh
make image                      # docker build -t paprika-adapter:local .
docker run --rm paprika-adapter:local version
```

Building elsewhere: `docker buildx build --platform linux/arm64 -t paprika-adapter:local --load .`

## 2. Credentials

```sh
install -d -m 700 ~/.config/paprika-adapter
install -m 600 paprika.env.example ~/.config/paprika-adapter/paprika.env
$EDITOR ~/.config/paprika-adapter/paprika.env      # PAPRIKA_EMAIL, PAPRIKA_PASSWORD, PAPRIKA_TZ
```

The file is read by the `docker` CLI and passed as environment; it is never mounted into the container and
never part of the image. Leave `PAPRIKA_ENABLE_WRITES=false` for now.

Where secrets live afterwards:

| What | Where | In git / logs / tool output |
|---|---|---|
| Email, password | `~/.config/paprika-adapter/paprika.env`, mode 600 | never |
| Bearer token | `token` in the `paprika-data` volume, mode 600 | never |
| Cache | `cache.sqlite` in the same volume; holds your recipes, no credentials | n/a |

## 3. First sync and verification (read-only)

```sh
make sync      # pulls everything once; prints per-collection state. The first run fetches every recipe.
make verify    # probes each Paprika read operation; prints a Markdown report
```

`make sync` must end with every collection `fresh`. Paste the `verify` output into
[PROTOCOL.md](PROTOCOL.md#status-on-your-account) together with your Paprika app version: that is the record
of what has actually been verified for your account. Any `FAILED` row or field drift: see [RUNBOOK.md](RUNBOOK.md).

## 4. Connect an MCP client

Add to the client's MCP configuration, replacing `1000` with the output of `id -u` and `YOU` with your user:

```yaml
mcp_servers:
  paprika:
    command: "docker"
    args:
      - "run"
      - "--rm"
      - "-i"
      - "--read-only"
      - "--cap-drop=ALL"
      - "--security-opt=no-new-privileges"
      - "--pids-limit=64"
      - "--memory=256m"
      - "--env-file=/home/YOU/.config/paprika-adapter/paprika.env"
      - "-v"
      - "paprika-data:/data"
      - "paprika-adapter:local"
    env:
      DOCKER_HOST: "unix:///run/user/1000/docker.sock"
    timeout: 120
    connect_timeout: 60
```

The client passes MCP servers only the variables listed under `env` plus a small baseline, not your shell
environment, which is why `DOCKER_HOST` is set explicitly. Then reload the client. The tools appear as `mcp_paprika_search_recipes`,
`mcp_paprika_get_meal_plan`, and so on. Ask the agent to call `mcp_paprika_sync_status`: `healthy` should be `true`.

Logs go to the container's stderr (the client's MCP log). They contain request paths, statuses and timings, never
headers, bodies or credentials.

## 5. Enable writes (second milestone)

Only after step 3 is clean and the read tools have been used for a while:

1. Take a backup: Paprika app → export, and `make export` (see [RUNBOOK.md](RUNBOOK.md#backup-and-export)).
2. Run the integration test against a disposable list ([RUNBOOK.md](RUNBOOK.md#verifying-writes)).
3. Set `PAPRIKA_ENABLE_WRITES=true` in `paprika.env`, and `PAPRIKA_APPROVAL=human` if the client must never
   write without you pressing approve. `/reload-mcp`.

Three more tools appear: `propose_meal_plan_entry`, `propose_grocery_items`, `commit_proposal`. To keep them
from one client profile without changing the server, use the client's filter:

```yaml
    tools:
      exclude: [propose_*, commit_proposal]
```

## Optional: keep the cache warm

While the client is running the adapter refreshes itself every `PAPRIKA_SYNC_INTERVAL`. To refresh when the client
is not running, add a systemd **user** timer:

```ini
# ~/.config/systemd/user/paprika-sync.service
[Service]
Type=oneshot
Environment=DOCKER_HOST=unix://%t/docker.sock
ExecStart=/usr/bin/docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --env-file=%h/.config/paprika-adapter/paprika.env -v paprika-data:/data paprika-adapter:local sync

# ~/.config/systemd/user/paprika-sync.timer
[Timer]
OnCalendar=hourly
Persistent=true
[Install]
WantedBy=timers.target
```

`systemctl --user enable --now paprika-sync.timer`. Several adapter processes can share the volume.

## Without Docker

`make linux-arm64` produces a static binary, `bin/paprika-adapter-linux-arm64`. Run it as an ordinary user:

```yaml
mcp_servers:
  paprika:
    command: "/home/YOU/bin/paprika-adapter"
    env:
      PAPRIKA_EMAIL: "you@example.com"
      PAPRIKA_PASSWORD_FILE: "/home/YOU/.config/paprika-adapter/password"   # mode 600
      PAPRIKA_TZ: "UTC"
```

State then lives in `~/.local/state/paprika-adapter` (override with `PAPRIKA_DATA_DIR`).

## Configuration reference

| Variable | Default | Meaning |
|---|---|---|
| `PAPRIKA_EMAIL`, `PAPRIKA_PASSWORD` | required | or `PAPRIKA_EMAIL_FILE` / `PAPRIKA_PASSWORD_FILE` naming a file |
| `PAPRIKA_ENABLE_WRITES` | `false` | register the propose/commit tools |
| `PAPRIKA_APPROVAL` | `auto` | `human` = refuse writes unless the client can show an approval prompt |
| `PAPRIKA_TZ` | process zone (UTC in the container) | decides "today" and the default week |
| `PAPRIKA_MAX_AGE` | `5m` | cached data older than this is re-checked before being served |
| `PAPRIKA_SYNC_INTERVAL` | `15m` | background refresh while serving; `0` disables |
| `PAPRIKA_DATA_DIR` | `/data` in the image, else `~/.local/state/paprika-adapter` | cache and token |
| `PAPRIKA_BASE_URL` | `https://www.paprikaapp.com` | https only, except loopback |
| `PAPRIKA_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Commands: `serve` (default), `sync`, `verify`, `export [-offline] DIR|-`, `tools`, `version`.
