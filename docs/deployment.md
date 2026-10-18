# Deployment

## Topology

The service runs **from this repository directory**. There is no separate deploy
directory any more. The old `/Users/ping/cli-proxy-api/` directory was removed
on 2026-09-22 after verifying the active process and launchd configuration use
this repository. Its final configuration was saved in the rollback backup below.

| | |
|---|---|
| launchd label | `com.routerforme.cli-proxy-api` |
| plist | `~/Library/LaunchAgents/com.routerforme.cli-proxy-api.plist` |
| program | `<repo>/cli-proxy-api` |
| config | `<repo>/config.yaml` |
| working directory | `<repo>` |
| stdout/stderr | `<repo>/logs/cpa-launchd.log` |
| listening port | `8888` (from `config.yaml`) |

Restart after a build:

```bash
go build -o cli-proxy-api ./cmd/server
launchctl kickstart -k gui/$(id -u)/com.routerforme.cli-proxy-api
```

### Environment variables the service depends on

Set in the plist, not in the shell. Dropping any of them changes behaviour
silently:

- `WRITABLE_PATH=/Users/ping/.cli-proxy-api` — decides where the management panel
  is read from. **Without it the panel falls back to `<repo>/static/`, which is a
  stale copy.**
- `CPA_SUPABASE_URL`, `CPA_SUPABASE_SERVICE_ROLE_KEY` — the usage-sync feature
  (F7). Without them the sync stops.
- `PATH` — so helper binaries resolve.

### Runtime artifacts (all gitignored)

The service writes into the working directory, so these live in the repo:

- `availability-state.json` — availability history (written continuously)
- `supabase-usage-outbox.json` — pending usage-sync records
- `logs/` — `cpa-launchd.log` (plist redirect) and the app's own file log
- `cli-proxy-api` — the running binary, replaced by every build

`config.yaml` holds real secrets and is gitignored. Do not commit it; keep a
redacted template in the repo instead.

Auth material, the request journal and the panel live outside the repo, under
`auth-dir` (`/Users/ping/.cli-proxy-api`):

- `auths`: OAuth credential JSON files
- `logs/request-journal/<date>.jsonl`: one record per upstream attempt
- `static/management.html`: the panel actually served (see below)

## The management panel: two gotchas

`managementasset.StaticDir` resolves in this order:

1. `MANAGEMENT_STATIC_PATH` (env)
2. `WRITABLE_PATH/static`
3. `<config file directory>/static`

With `WRITABLE_PATH=/Users/ping/.cli-proxy-api`, the live panel is
`/Users/ping/.cli-proxy-api/static/management.html` — the build that carries the
local features (`user-side`, `gantt`, `opencode`, `模型巡检`). Verify a deployed
panel by grepping for those markers; a panel **without** them is upstream's.

**Gotcha:** if the panel file is missing at the resolved path,
`serveManagementControlPanel` synchronously downloads upstream's panel from
GitHub — and `disable-auto-update-panel` does **not** prevent this (that flag
only gates the 3-hourly refresh). A missing panel is therefore silently replaced
by upstream's, losing the local UI. Keep a backup of the panel and restore it
rather than letting the server re-download.

## Rollback

Everything from the 2026-09-22 migration is in
`~/.cli-proxy-api/rollback-20260922/` (mode 700):

- `com.routerforme.cli-proxy-api.plist.bak` — the previous plist, pointing at
  `/Users/ping/cli-proxy-api/`
- `cli-proxy-api.repo-old-20260922114641` — the repo's previous binary
- `config.yaml.repo-before-migration` — the repo's previous (stale) config
- `availability-state.json.repo-before-migration`,
  `management.html.repo-before-migration`

The old deployment directory was deleted on 2026-09-22. Its final configuration
is preserved as `config.yaml.old-deploy-before-removal` in this backup directory.
The current repository configuration has subsequent changes and was not replaced.

The backed-up plist points at the deleted directory and cannot be restored as a
working rollback by itself. Any rollback now requires restoring a compatible
binary and configuration and checking the launchd paths before restarting.

## History worth knowing

- A second, stale service `com.cpa.cli-proxy-api` (plist from 2026-07-17) pointed
  at an old repo binary and could never bind `8888`, because the
  `com.routerforme` instance held it. `KeepAlive` turned that into a respawn loop
  — 38,369 restarts and an 11 MB `/tmp/cpa.log`. It was unloaded and its plist
  removed on 2026-09-22. **Two instances must never share `auth-dir`**: they would
  both refresh the same OAuth tokens and invalidate each other.
- The two `config.yaml` copies had diverged by 975 lines (26 vs 24
  `openai-compatibility` channels, and only the deploy copy had
  `availability-retention-hours` / `request-journal-keep-days`). The deploy copy
  was adopted as authoritative; the repo copy had no settings of its own.
- `API server started successfully on: :PORT` is printed unconditionally 100 ms
  after the server goroutine starts — it does **not** mean the port was bound.
  Check with `lsof -nP -iTCP:<port> -sTCP:LISTEN`, and note that the real bind
  error goes to the file log, not stdout, when `logging-to-file: true`.
