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
(`ae6a978d`, based on v7.2.140). Scratch branch `merge/v7.3.11`, commit
`4bb8252d`. **WIP — not for deployment.**

### Scale

- 478 upstream commits, 773 files, +166k lines.
- 18 conflicted files, 39 conflict hunks, 1553 lines of conflict region.
- Every conflicted file had local non-merge commits touching it, so all 18 were
  genuine "both sides changed it" conflicts, not merge leftovers.

### Test result

`go build ./...` passes. 93 of 105 test packages pass; 6 packages fail.

Seven tests fail in total. One is pre-existing on `enterprise-base` and
unrelated (`TestListModelProbeResultsReturnsPersistedEvidence`). The other six
are semantic clashes between the local features and upstream's new models — each
needs a design decision, not a conflict resolution. All six were confirmed to
pass against a clean `origin/main` checkout, so they are not upstream bugs.

### Open item 1 — antigravity registration blocks on the model fetch

*Failing:* `TestAntigravityModelRegistration_AsyncNonBlocking`

Upstream requires `registerModelsForAuth` to return in under 500 ms and does its
capability probing asynchronously (`asyncProbeAntigravityCapabilities`). The
local authoritative-catalog fetch (`fetchAntigravityModelsForAuth`, called from
`sdk/cliproxy/service_models.go`) runs synchronously inside registration, so
registration blocks on a network round trip — 10 s in the test.

*Decision needed:* move the local catalog fetch behind the same asynchronous
probe machinery upstream uses, so registration stays non-blocking and the
fetched catalog is applied to the registry when it arrives. This is the
recommended direction; it is also the larger change, because the local feature
replaces the client's whole model list rather than mutating capabilities in
place.

### Open item 2 — stream bootstrap commit rule (F6 vs upstream)

*Failing:* `TestCodexModelLevelCoolingPreservesSiblingModel`,
`TestCodexTerminalQuotaCoolsAccountAcrossModels`,
`TestCodexIncompleteStreamIsNotTypedAsInvalidRequest`

Upstream commits the stream bootstrap as soon as **any** payload arrives. The
local `readStreamBootstrapWithTimeoutFor` keeps buffering payloads that are only
protocol metadata and commits on the first **client-visible** payload
(`isStreamContentPayload`).

Consequence, verified on `TestCodexModelLevelCoolingPreservesSiblingModel`: a
Codex stream sends `response.created` and then a terminal `usage_limit_reached`.
Under the local rule the stream still counts as uncommitted, so the error becomes
a retryable bootstrap failure and the manager rotates to another credential — the
upstream log shows two upstream attempts (`quota-high` then `quota-low`) where
upstream makes one. The client sees a successful response instead of the quota
error. `response.created` is not in the local content set for
`FormatOpenAIResponse`; `response.completed` is.

Two upstream tests and the local `stream_bootstrap_timeout_test.go` lock the two
rules against each other.

*Decision needed:* whether a Responses-API stream should commit on
`response.created`. Either widen the local content set to include it (matches
upstream; narrows the retry window for Codex streams), or keep the local rule and
make the caller surface terminal errors instead of rotating (keeps the wider
retry window; needs a caller change upstream does not have).

### Open item 3 — credential scope of quota errors (F7 vs upstream)

*Failing:* `TestExecuteStreamWithAuthManager_ReportsBootstrapRetryModelCooldown`
(local), `TestTemporary403CooldownRecoversAfterExpiry` (local),
`TestManagerSessionAffinityAliasCooldownPreservesSelection` (upstream)

Three disagreements in the cooldown model. The first two were root-caused; the
third is reported from the resolver's analysis and not independently reproduced:

- Upstream's new `ApplyClientModelProjections` projects any live model cooldown as
  `Suspended`, so `registry.GetModelCount` returns 0 immediately after a 403
  cooldown. The local 403-recovery test expects the model to stay registered until
  the cooldown expires. Confirmed by disabling the projection call in a scratch
  copy, after which the test passes.
- The local `authDegradationPenalty` (2 recent failures → priority −5) demotes a
  credential below a lower-priority peer, which breaks upstream's session-affinity
  test.
- The local quota classification returns 503 where an upstream test expects 429.

*Decision needed:* whether the degradation penalty participates in cross-tier
ordering or only within a tier, and whether model-level cooling should suppress
the local credential-scoped quota propagation.

### Verification still outstanding

The merge has not been exercised end to end. Before it can be deployed it needs
the three layers:

- **API** — the management endpoints (`/v0/management/user-side-availability`,
  `/trend`, `/requests`, `/opencode/usage`) and the control-plane enforcement
  paths (IP ACL, model whitelist, monthly budget, TPM) against a live instance.
- **GUI** — the management panel: the availability tab, the Gantt waterfall, the
  trend chart's window selector, and the auth-file cooldown display.
- **DB** — the control-plane counters (`auth:<sha256>`, `cp:budget:key:<id>:<YYYY-MM>`,
  `cp:tpm:usage:<keyHash>`, `cp:rpm:<keyHash>`) and the request journal on disk.
