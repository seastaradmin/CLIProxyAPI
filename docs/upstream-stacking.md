# Stacking the local feature set on upstream

This branch (`enterprise-base`) is an enterprise gateway built on top of
upstream CLIProxyAPI. Upstream moves fast — 478 commits landed between our
v7.2.140 base and v7.3.11 — so the local work is kept as a set of separable
feature items that can be re-stacked on each new upstream `main`.

This document is the inventory of those items and the procedure for re-stacking
them. Keep it current: when a feature item is added, changed, or dropped, update
the table.

## Feature inventory

| ID | Feature | Commit | Main files |
|----|---------|--------|------------|
| F1 | Availability monitoring, model probing, request journal | `1123e9c5` | `internal/requestjournal/`, `internal/api/handlers/management/`, `internal/redisqueue/`, `sdk/cliproxy/usage/` |
| F2 | Per-key model protocols, endpoints, fork alias | `58714f8d` | `internal/config/`, `internal/runtime/executor/`, `sdk/cliproxy/auth/`, `internal/watcher/`, `internal/auth/xai/` |
| F3 | `cpactl` management CLI and one-click service scripts | `32b2209a` | `cmd/cpactl/`, `cpa-oneclick/` |
| F4 | User-side availability: attempt chains and trend | `ecfe090e` | `internal/requestjournal/`, `internal/api/handlers/management/`, `sdk/cliproxy/usage/`, `CONTEXT.md` |
| F5 | Control-plane key validation and usage metering | `2c8d189a`, `7e665b13`, `6739aa5b` | `internal/controlplane/`, `internal/probe/`, `cmd/server/main.go` |
| F6 | Runtime hardening: stall watchdog, `/v1` fallback, thinking downgrade | `cbbe544a` | `internal/runtime/executor/`, `internal/runtime/executor/helps/`, `internal/thinking/` |
| F7 | Quota vs rate-limit classification, network-error attribution | `24ea493c` | `sdk/cliproxy/auth/`, `sdk/cliproxy/usage/` |
| F8 | Image fingerprint cache, auth-file excluded/quota handlers | `fd3c2e97` | `internal/cache/`, `internal/api/handlers/management/`, `sdk/api/handlers/` |
| F9 | OpenCode usage endpoint | `f41aad75` | `internal/api/handlers/management/opencode.go`, `internal/api/server_management.go` |
| F10 | Network timeouts classified as 504 | `4528e29e` | `internal/clienterror/` |

Feature items F1–F8 came from the original self-developed layer; F9 and F10 are
later additions. F5 gained the IP ACL / model whitelist / budget / TPM
enforcement in `7e665b13` and the TPM settlement in `6739aa5b`.

### Which items are conflict-prone

Items that modify upstream files (rather than adding new ones) are the ones that
cost real merge effort. Ranked by exposure:

- **F7 (quota classification)** — rewrites the same cooldown decision chain
  upstream keeps changing (`sdk/cliproxy/auth/conductor_cooldown.go`). Highest
  risk; see the open items in the v7.3.11 report below.
- **F2 (per-key routing)** — touches the executor and selector paths upstream
  refactors constantly.
- **F6 (runtime hardening)** — the stream bootstrap read in
  `sdk/cliproxy/auth/conductor_stream.go` and the executor stream loops.
- **F1 / F4 (journal and availability)** — mostly new files, but they add
  management handlers and a usage reporter that upstream also edits.

Items F3, F9 and F10 are additive or self-contained and have not conflicted so far.

## Re-stacking procedure

Work on a throwaway branch, never directly on `enterprise-base`:

```bash
git fetch origin
git worktree add -b merge/vX.Y.Z .worktrees/merge-vX.Y.Z enterprise-base
cd .worktrees/merge-vX.Y.Z
git merge origin/main
```

`.worktrees/` is gitignored, so the scratch tree stays out of the repo. Then:

1. **Enumerate the conflicts before resolving anything.**
   ```bash
   git diff --name-only --diff-filter=U | sort
   ```
   For each conflicted file, ask whether the local side has a non-merge commit
   touching it. If not, the conflict is a leftover from an earlier merge and can
   usually be resolved by taking upstream.
   ```bash
   git log --no-merges --oneline <merge-base>..enterprise-base -- <file>
   ```

2. **Classify each hunk before editing.** Three shapes, in order of frequency:
   - *Additive on both sides* (different fields, cases, tests) — keep both.
   - *Upstream restructured, local added a feature inside* — take upstream's
     structure and re-apply the local feature inside it.
   - *Both changed the same logic* — a design decision, not a merge conflict.
     Do not guess. These are the ones to escalate; list them and their tests.

3. **Do not trust a clean build.** A mechanically resolved merge compiles and
   still gets the behaviour wrong. Run the full suite and compare against a
   baseline of `enterprise-base` before the merge:
   ```bash
   go build ./...
   go test ./...            # note: sdk/cliproxy takes ~10 minutes
   ```
   Any test that passes on both `origin/main` and `enterprise-base` but fails on
   the merge is a resolution bug or a real semantic clash. Investigate which.

4. **Check for auto-merge artifacts outside the conflict markers.** Git can
   merge both sides' additions into one file and produce something that does not
   compile — a duplicated struct field, a doubled brace. In the v7.3.11 merge
   this happened in `sdk/cliproxy/usage/manager.go` (duplicate `BaseURL`) and in
   two helper functions whose closing braces the merge consumed.

5. Run `gofmt -w` on everything touched, then commit on the scratch branch with
   a message listing what is still failing and why.

## v7.3.11 merge report

Merged `origin/main` (`ffe6ad3c`, v7.3.11, 2026-09-21) into `enterprise-base`
(`ae6a978d`, based on v7.2.140), and landed the result on `enterprise-base` at
`f0b9e28b`. The scratch branch `merge/v7.3.11` keeps the same work.

**Follow-up: v7.3.12 merged the same day, 0 conflicts.** 15 upstream commits
(`ffe6ad3c..55566294`, mostly executor perf work, a `grok-4.7` model definition
and a Responses tool index), landed at `7ff46cca`, green on the first test run.
`enterprise-base` is now 0 behind / 31 ahead of `origin/main`.

That contrast is the point of this document: the cost of a bump tracks how long
you waited, not how much upstream shipped. A month of drift cost 39 conflict
hunks; a few hours cost none.

### Scale

- 478 upstream commits, 773 files, +166k lines.
- 18 conflicted files, 39 conflict hunks, 1553 lines of conflict region.
- Every conflicted file had local non-merge commits touching it, so all 18 were
  genuine "both sides changed it" conflicts, not merge leftovers.

### Test result

`go build ./...` passes and **all 99 test packages pass** — the same result as a
clean upstream checkout.

Resolving the conflicts mechanically gets a build but not correct behaviour: it
left seven failing tests. Six were semantic clashes where the local features and
upstream's new cooldown/stream model genuinely disagreed; the seventh was a
pre-existing time-dependent bug on `enterprise-base`. All seven are now fixed.
Details below, because the same three clashes will resurface at the next
upstream bump.

### Resolution 1 — antigravity registration is now asynchronous

`registerModelsForAuth` no longer calls `fetchAntigravityModelsForAuth`
synchronously. It returns the static list like upstream, and the catalog fetch
runs on `antigravityProbeWg` inside `asyncProbeAntigravityCapabilities`, which
applies the catalog through an epoch-guarded replace and falls back to upstream's
capability-hint mutation when the response carries no model map.
`WaitAntigravityProbes()` still waits for it, so the local catalog test passes
unmodified. This also removed a deadlock: the synchronous fetch blocked the test
goroutine before the probe mocks could be released, which hung
`TestAntigravityAsyncProbe_DiscardsStaleProbeWhenAuthReRegistered` until the
10-minute panic.

Two things to know about the result:

- The endpoint-level capability-hints cache (`fetchAntigravityModelCapabilityHintsForAuth`,
  5-minute TTL, singleflight, auth-failure backoff) is no longer on the
  registration path — hints now come from the same per-credential catalog
  response. The helper and its tests are intact. An authoritative catalog is
  per-credential while that cache is keyed by base URL and proxy, so serving
  catalogs from it would leak one account's catalog to another. To restore
  upstream's dedup, wrap the catalog fetch in the existing
  `antigravityCapabilityGroup` with a per-(auth, endpoint) key.
- An authoritative-but-empty catalog (`"models": {}`) is treated as a no-op
  rather than an unregister, so a malformed-looking response cannot wipe a
  registration.

### Resolution 2 — `response.created` commits the stream

Both copies of the content-window rule (`sdk/cliproxy/auth/conductor_stream.go`
and `sdk/api/handlers/handlers_stream.go`) treated `response.created` as protocol
metadata and kept buffering. A Codex stream that announced its response and then
failed therefore looked uncommitted: the manager retried on another credential
(the log showed two upstream attempts where upstream makes one) and the handler
reported a pre-stream JSON error for a truncated stream, typed
`invalid_request_error` — a type that tells every downstream client not to retry.

`response.created` now counts as client-visible content for `openai-response` in
both places, which matches upstream's effective behaviour (upstream commits on
the first payload) while keeping the content window for the metadata-only cases
the local rule was written for. Note there are **two** copies of this rule — a
future change must touch both.

### Resolution 3 — the cooling model follows upstream

- `authPriority` returns the configured priority only, so the local
  `authDegradationPenalty` can no longer move a credential between tiers, which
  is what upstream's session-affinity test requires. The penalty is re-scoped to
  intra-tier ordering (`orderWithinTier`) in the selector path. Caveat: the
  penalty does not apply on `scheduler.go`'s shard fast path, which keeps
  upstream's ordering and its O(1) selection.
- Two local tests asserted behaviour upstream deliberately changed and were
  updated to upstream's semantics rather than papering over the merge:
  `TestTemporary403CooldownSuspendsModelThenRecovers` now asserts upstream's
  projection (model suspended while the cooldown is live, recovered after
  reconciliation), and `TestExecuteStreamWithAuthManager_ReportsBootstrapRetryAuthUnavailable`
  now asserts `auth_unavailable`/503. The latter was confirmed against a clean
  `origin/main` checkout to be upstream's behaviour rather than a resolution
  error, which is exactly the check to repeat for any local test that fails only
  after a merge.

### Pre-existing bug fixed along the way

`TestListModelProbeResultsReturnsPersistedEvidence` was failing on
`enterprise-base` before this merge. It goes through the HTTP handler, which
loads probe results with `time.Now()` and drops evidence older than the 30-day
retention window, but its fixture used a fixed 2026-07-25 timestamp — so it
started failing by itself about a month after it was written. The fixture is now
anchored to now.

### Verification still outstanding

The merge is green at unit and integration level but has not been exercised end
to end. Before deployment it needs the three layers:

- **API** — the management endpoints (`/v0/management/user-side-availability`,
  `/trend`, `/requests`, `/opencode/usage`) and the control-plane enforcement
  paths (IP ACL, model whitelist, monthly budget, TPM) against a live instance.
- **GUI** — the management panel: the availability tab, the Gantt waterfall, the
  trend chart's window selector, and the auth-file cooldown display.
- **DB** — the control-plane counters (`auth:<sha256>`, `cp:budget:key:<id>:<YYYY-MM>`,
  `cp:tpm:usage:<keyHash>`, `cp:rpm:<keyHash>`) and the request journal on disk.

