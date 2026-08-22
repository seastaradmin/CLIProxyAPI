# CLIProxyAPI — Availability & Routing

Glossary for the availability, routing, and retry domain. Work alongside
`docs/channel-key-model-availability-todo_CN.md` (the capability-edge TODO) and
`docs/retry-and-auth-switching_CN.md` (retry/cooldown reference).

## Language

**Upstream Attempt**:
One executor call to an upstream provider. The unit the existing availability
cache and request journal are sampled on; one retried user request produces
several of these.
_Avoid_: request, call, probe

**User Request**:
One inbound HTTP request to the proxy. Assigned a single `RequestID` by the Gin
middleware; every upstream attempt it triggers (retries, credential rotation)
runs under the same `RequestID`.

**Request-Level Outcome**:
The final result of a User Request once retries and rotation are exhausted:
`resolved` (no hard error delivered to the user) or `hard-failed`. Derived by
grouping Upstream Attempt records by `RequestID`: a request is `resolved` when
its latest-timestamped attempt succeeded. (Known approximation for requests that
fan out into several parallel upstream calls — see ADR-0001.)
_Avoid_: "availability of the request"

**User-Side Availability** (用户侧可用性 / 真实可用性):
A monitoring metric over User Requests that reached upstream: the share that
ended `resolved` (the retry/rotation safety net caught them) vs `hard-failed`.
Distinct from per-attempt API availability — it answers "did the user actually
get served, not just did one upstream call succeed."
_Avoid_: upstream availability, probe availability

**Safety Net** (安全网):
The combined retry / backoff / credential-rotation machinery
(`sdk/cliproxy/auth/`) that absorbs transient upstream failures before the user
sees them. User-Side Availability measures whether it succeeded.

**Capability Edge**:
A `channel × credential × model × protocol` route. The aggregation dimension
for both per-attempt availability and User-Side Availability.

**Attempt Chain** (尝试链):
The ordered, timestamped timeline of every Upstream Attempt that one User Request
made (its retries and credential/provider switches), grouped by `RequestID`. The
unit the failure waterfall displays. See ADR-0002.
_Avoid_: session log, request history

**Switch** (切换):
A marker in an Attempt Chain between two consecutive attempts whose `provider` or
`auth_index` differs. Inferred by comparing adjacent attempts — the system does
not record the switch action itself. Model changes are not flagged (no model
downgrade today). See ADR-0002.

**User-Side Trend**:
User-Side Availability plotted over time, bucketed per window
(`15m→1m / 1h→5m / 6h→15m / 24h→1h / 7d→6h / 30d→24h`). The top chart of the
`用户可用性` tab. See ADR-0002.
