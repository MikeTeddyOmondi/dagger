# LLM subscription OAuth: token lifecycle cleanup

Status: audit complete, one prototype commit staged, four steps of work
remaining. This document is the handoff — it is meant to be read cold.

## The symptom

A user running a long `dagger agent` / `dagger shell` session against Claude
Code (Anthropic) or Codex (ChatGPT) subscription auth starts getting `401
authentication_error` partway through, and stays broken until they restart
the CLI. A client-side refresh mechanism already exists and is correctly
wired; it just never gets a chance to fire.

## How it works today

| Step | Where |
|---|---|
| Creds persisted in `~/.config/dagger/config.toml` (`auth_token`, `refresh_token`, `token_expiry`) | `internal/cmd/dagger/llmconfig/config.go` `Provider` |
| Refresh-if-expired at CLI start, then `os.Setenv("ANTHROPIC_AUTH_TOKEN", …)` | `internal/cmd/dagger/llm_config.go` `applyLLMConfigEnv` (via `cobra.OnInitialize`) |
| Refresh-on-demand hook | `internal/cmd/dagger/llm_config.go` `secretprovider.RegisterEnvRefresher(…)` |
| Hook fires inside every `env://` resolution | `engine/client/secretprovider/env.go` `envProvider` |
| Engine asks the client `secret(uri:"env://X"){plaintext}` | `core/llm.go` `LLMRouter.LoadClientConfig` → `loadSecret` |
| Resolution RPC engine→client | `core/secret.go` `Secret.plaintext` → `secrets.GetSecret` |
| Lands as a plain string on the router | `core/llm.go` `LLMRouter.AnthropicAuthToken` / `OpenAICodexAuthToken` |
| Router → endpoint | `core/llm.go` `routeAnthropicModel` / `routeCodexModel` |
| **Baked into the SDK client at construction** | `core/llm_anthropic.go` `option.WithAuthToken`; `core/llm_openai_codex.go` `option.WithAPIKey` + `chatgpt-account-id` from the JWT |
| **Memoized for the rest of the conversation** | `core/llm.go` `(*LLM).Endpoint`, and `Clone` copies the pointer |

Everything *upstream* of the last two rows already re-resolves correctly:
`secret` is `dagql.PerCallInput` (`core/schema/secret.go`), `plaintext` is
`DoNotCache`, so every `LoadClientConfig` genuinely round-trips to the client
and the CLI-side refresher genuinely runs.

## Root cause

Two lines, both in `core/llm.go`:

1. `(*LLM).Endpoint` short-circuits on `llm.endpoint != nil`.
2. `Clone()` does `cp.endpoint = llm.endpoint`.

The router load is the *only* thing that consults the refresher hook. After
the first `Endpoint()` call on a persistent node, the router is never loaded
again, so the token is frozen for the session.

Fixing the memo alone is not enough: the token is captured inside the
provider SDK client at construction, so mutating `LLMEndpoint.AuthToken` in
place would still send the old value.

### Which call sites pin it

Not `step()` — it does `llm = llm.Clone()` first, so its memo lands on a
throwaway and dies. The culprits are the *observation* fields in
`core/schema/llm.go`: `model`, `provider`, `contextWindow`, `reasoningEffort`
all call `llm.Endpoint(ctx)` on the receiver with no clone, memoizing onto
the dagql-cached node. The CLI calls them constantly —
`internal/cmd/dagger/llm.go` (session construction) and
`internal/cmd/dagger/session_agent.go` (`setLLM`, `updateStatusLine`, and
before *every* prompt turn).

Sequence: CLI starts → refresh+export token T once → `.Model()` builds the
endpoint and SDK client with T → `withPrompt` clones, inheriting the endpoint
→ every turn for the rest of the session sends T.

Only `WithModel` and `WithReasoningEffort` reset the memo — which is why
typing `.model` appears to fix it.

### Parallel agents

Workers are **correct today by accident**. `modules/staff` / `modules/delegate`
compose from the bare `llm()` node (`core/schema/agents.go`), whose endpoint
is usually still nil, so each worker's `step()` re-resolves per turn and does
pick up refreshes. But `llm` is `WithInput(dagql.PerSessionInput)` and not
`DoNotCache` (`core/schema/llm.go`) — one shared `*LLM` per session. The
moment anything resolves `model` on that bare node, the chief and every
worker share one `*LLMEndpoint`, one SDK client, one frozen token. There is
no propagation mechanism either way.

## Defect inventory

Ordered by severity. Everything below was confirmed by reading the code;
items marked (T) were additionally reproduced by a test.

### Permanent logout — loses the *refresh* token, not just the access token

- **Cross-process clobber (T).** `oauthRefreshMu` (`llmconfig/config.go`) is
  process-local; the flock is taken *inside* `Save`, not around
  load→refresh→persist. `Save` rewrites the whole `[llm]` section from a
  stale snapshot. Two `dagger` processes — a `dagger call` beside a live
  `dagger agent`, a Makefile, a CI matrix — and one reverts the other's
  rotated token. Next refresh: `invalid_grant`, permanently. Easy to hit
  because `applyLLMConfigEnv` runs on *every* command via
  `cobra.OnInitialize`.
- **Refresh token erased when the response omits it (T).**
  `oauth.go` `RefreshOAuthToken` and `oauth_openai.go`
  `RefreshOpenAIOAuthToken` both do `updated.RefreshToken =
  tokenResp.RefreshToken` unconditionally. RFC 6749 §5.1 makes the field
  optional; endpoints that don't rotate omit it. Result: `""`, then
  `"no refresh token available"` forever.
- **`RefreshOAuthTokensIfNeeded` drops successful work (T).** It returns on
  the first provider's error *before* `cfg.Save()`, so a provider that
  already spent its one-time grant loses the result. Which provider loses is
  decided by Go's randomized map order.
- **`Save` mkdirs the wrong directory (T).** It creates `ConfigRoot` instead
  of `filepath.Dir(ConfigFile)` — wrong whenever `DAGGER_CONFIG` points
  outside the XDG root. `UpdateFile` gets this right. Such a user's *first*
  refresh spends the grant and fails to persist.

### Refresh storm

- **`expires_in` absent or ≤ 300 (T).** The 5-minute margin is baked into the
  persisted value at write time: `TokenExpiry = now + expires_in*1000 -
  300000`. With `expires_in` omitted that is `now − 300000`, and
  `IsTokenExpired` (`now >= TokenExpiry`) is true immediately → a refresh on
  every secret resolution, each rotating the refresh token. Note a single
  resolution costs **two** hook invocations: `secretSchema.secret` resolves
  the plaintext once to derive the argon2 cache-key handle, then `.plaintext`
  resolves again.

### Hangs and silence

- **No timeout, no context.** Bare `http.Post` with the default client in
  both refresh paths, while holding `oauthRefreshMu`. `envProvider` awaits
  the hook synchronously inside the client's `GetSecret` handler; the engine
  is blocked on that RPC; `LoadConfig` `eg.Wait()`s on ~21 lookups. A hung
  token endpoint hangs the session, uncancellably — the hook signature
  discards its `ctx` and `RefreshOAuthProviderIfNeeded` takes none.
  (`FetchSubscriptionType` *does* use a 10s client, so this looks like an
  oversight.)
- **Errors swallowed (T).** `envProvider` does `_ = r(ctx, name)`. A revoked
  token, unwritable config, or network blip all degrade to "serve the stale
  token, return nil". No logging on that path at all.
  `applyLLMConfigEnv`'s `slog.Warn` covers startup only and fires before any
  `slog.SetDefault`, so it goes to raw stderr outside the frontend — never a
  span, never in `dagger trace`.
- **401 is permanent, and a retry could not help.** Neither `IsRetryable`
  matches 401 (`llm_anthropic.go`, `llm_openai_codex.go`), and
  `sendQueryWithRetry` hoists `client := ep.Client` *outside* the backoff
  closure, so every attempt would reuse the dead token. No "your login
  expired" hint anywhere.

### Smaller

- **Explicit env token clobbered (T).** `applyLLMConfigEnv` honors a
  user-exported `ANTHROPIC_AUTH_TOKEN` via `setIfEmpty`, then the refresher
  hook `os.Setenv`s unconditionally with the config's token — even when
  nothing was refreshed (`RefreshOAuthProviderIfNeeded` returns the stored
  token when `changed` is false).
- **`Enabled` ignored (T).** `RefreshOAuthProviderIfNeeded` never checks it,
  unlike `applyLLMConfigEnv`. And an injected OAuth token *clears*
  `ANTHROPIC_API_KEY` (`core/llm.go` `LoadConfig`, the
  `anthropicTokenSet && !anthropicKeySet` branch), so a disabled subscription
  hijacks an API-key setup.
- **`("", nil)` conflates** "provider absent", "not OAuth", and "OAuth with
  no token", so nothing can distinguish a broken config from an
  inapplicable one.
- **Provider dispatch is by literal name** (`refreshProviderToken`): anything
  not named `openai-codex` gets the Anthropic client ID and JSON flow.
- **`Remove()` leaves a stray `config.toml.lock`.**
- **Unsynchronized read of `llm.endpoint` in `Clone()`** against the mutexed
  write in `Endpoint()` — a real race between a detached agent loop and the
  status-line path. (`WithModel`/`WithReasoningEffort` lock the *clone's*
  brand-new mutex, which protects nothing.)
- **Stale comment**: `applyLLMConfigEnv`'s doc says Codex "is not yet wired…
  so its token is not exported". It is exported.
- **Out of scope but same bug class**: `internal/cloud/auth` `Token()` has
  the same cross-process race, and worse — it builds a fresh
  `authConfig.TokenSource(...)` per call, so there is no in-process dedupe
  either.

## Already staged (3 commits)

These are on top of the base checkout and are what you are inheriting.

1. `core/llm: prototype per-request credential source` — adds
   `core/llm_credential.go` (`CredentialSource`, `cachedCredential` with TTL
   + single-flight + stale-on-error fallback, `credentialTransport`,
   `applyBearer`/`applyCodexBearer`), wires `LLMEndpoint.AuthTokenSource`,
   `LLMRouter.reload{Anthropic,Codex}AuthToken` via a `reloader(getenv, key)`
   closure, pins that closure to the loading client in `LoadClientConfig`,
   and inserts the transport in `otelHTTPClient`. Plus
   `core/llm_credential_test.go`. **Marked in its message as a spike, not
   merge-ready** — step 2 below de-prototypes it.
2. `core/llm: drop now-unused unparam directive` — lint fallout.
3. `core/llm: test endpoint memoization pins the token` — adds
   `core/llm_auth_test.go`: a real `Endpoint()` resolution against a mocked
   secret schema (`PerCallInput` + `DoNotCache`, matching
   `core/schema/secret.go`) with a rotatable env, showing the memo survives
   a refresh and rides `Clone` into prompts, responses and parallel forks;
   the `step()`-shaped counterpart where resolving on a clone leaves the
   receiver free to re-derive; and httptest-backed checks that both SDK
   clients keep sending the token they were built with, with an
   expired-token 401 classified non-retryable by both.

`go build ./core/... ./engine/... ./internal/cmd/...` and `go test ./core/`
are green on this base.

> Note: a fourth worker produced ~15 client-side tests (including two
> real-subprocess cross-process ones) that asserted the intended behavior and
> therefore failed against today's code. Its sandbox snapshot became
> unreplayable (overlay mount options too long) and the tests were lost.
> They need rewriting — but written *alongside* the fixes they should pass,
> which is better anyway. The defect inventory above records exactly what
> they covered.

## Remaining work

### Step 1 — cross-process refresh race (client, independent)

Do the whole load→refresh→persist inside the flock, and **re-read after
acquiring it** so a process that lost the race picks up the winner's fresh
token instead of spending a dead one. `UpdateFile` already has the right
shape; `RefreshOAuthProviderIfNeeded` and `RefreshOAuthTokensIfNeeded` should
route through it. Keep `oauthRefreshMu` as the in-process fast path.

Fold in the rest of the permanent-logout class while you are in these
functions, each as its own commit:

- keep `RefreshToken` when the response omits it;
- persist providers that already succeeded even if a later one fails;
- `Save` mkdirs `filepath.Dir(ConfigFile)`;
- bound both refresh paths with a `context.Context` and a timeout, and plumb
  the `ctx` the env-refresher hook already receives;
- surface refresh failures — at minimum an `slog.Warn` on the on-demand path,
  ideally as a span event so it reaches `dagger trace`;
- honor `Enabled`, and stop clobbering an explicitly-exported token.

### Step 2 — de-prototype the per-request credential source (engine)

Take the staged spike to merge quality:

- **Session-scoped resolution context.** The spike's reloader re-`Select`s
  on the `dagql.Server` using `req.Context()`. Capture a
  `query.Server.SessionScopedContext(ctx)` in `loadLLMRouter` instead (the
  same idiom `setupLocalTunnel` uses for the local-LLM tunnel) and use it as
  the base for credential resolution, with a bounded timeout. A detached
  agent loop must not depend on the ctx of whichever request first routed
  the endpoint.
- **401 recovery.** On an expired-credential 401: invalidate the cached
  credential and retry once. Note `sendQueryWithRetry` currently hoists
  `client := ep.Client` outside the backoff closure — that must move inside,
  or the retry is pointless.
- Rewrite the commit message (it currently says "prototype only").
- Fix the `Clone()`/`Endpoint()` data race while you are here.

**Hard constraint: do not "fix" this by reloading the whole router per
request.** `LoadConfig` resolves ~21 variables, each costing 2 `GetSecret`
RPCs plus an argon2 hash, and `loadLLMRouter` loads twice when the main and
parent clients differ — ~80 round-trips. The endpoint memo is load-bearing
for latency; it just must not be memoizing *credentials*.

### Step 3 — carry expiry alongside the token

So the engine's cache can expire precisely instead of guessing with a 30s TTL.

**Config change.** Add `token_expires_at` (unix ms, the *true* expiry, no
margin baked in). Apply the safety margin at *check* time, not write time.
Migration: when `token_expires_at` is 0 and the legacy `token_expiry` is
non-zero, treat the true expiry as `token_expiry + margin` (undoing the
baked-in margin). Write only the new field going forward; keep reading the
old one. This also kills the `expires_in`-absent refresh storm, since a
missing `expires_in` should mean "unknown expiry", not "expired".

**Env contract** (both halves must agree):

- Client exports `ANTHROPIC_AUTH_TOKEN_EXPIRES_AT` and
  `OPENAI_CODEX_AUTH_TOKEN_EXPIRES_AT`, RFC 3339 UTC, the true access-token
  expiry. Absent/empty means unknown.
- Engine's `CredentialSource` resolves the token var **first**, then the
  expiry var, in one call — the refresher hook fires on the token var and
  updates both, so ordering matters.
- Cache validity horizon = `min(expiresAt − margin, now + maxTTL)`; unknown
  expiry falls back to the existing 30s TTL.

### Step 4 — proactive refresh (this is "Option 2")

**Design decision, made deliberately — read this before implementing.**

The original sketch for Option 2 was a *push* channel: the CLI refreshes on a
timer and pushes the new token into the live session, e.g. by rebinding a
`setSecret` handle (which is already a mutable, session-resident, name-keyed
credential slot — `SetSecretHandle` is `HMAC(scope, name)`, content
independent, and `BindSessionResource` overwrites).

**Do not build that.** Once step 3 lands, pull + exact-expiry caching gets
the same properties for far less surface:

- the engine caches the credential until its real expiry, so it re-pulls
  roughly once per token lifetime rather than per request;
- a client-side timer that refreshes *before* expiry means that pull returns
  an already-fresh token instead of triggering an inline refresh on the
  critical path;
- every agent sharing an endpoint shares the cache, and independent endpoints
  each pull at most once per lifetime;
- nested clients, CI and plain API keys keep working with **zero** new API
  surface.

Push's only real win is eliminating a ~1-per-hour RPC, and it costs a new
session-mutable credential slot plus care about which module scope owns the
name. Not worth it. Revisit only if profiling says otherwise.

So step 4 is: **a background goroutine in the CLI that refreshes each OAuth
provider ~5 minutes before expiry** and updates the process env, alongside
`applyLLMConfigEnv`. It must survive idle periods in `dagger shell`/`agent`,
tear down cleanly, and no-op when no OAuth provider is configured. Keep the
on-demand hook as the fallback — it is what covers a laptop resuming from
sleep with a long-dead timer.

## Options considered and rejected

- **Proxy all LLM traffic through the client** (so the engine never sees the
  token). Mechanically feasible reusing the local-LLM tunnel's
  `SocketKindHostIP` + `sshforward` machinery, but that tunnel is a *raw TCP
  forward* with TLS end-to-end, so you would have to invert it: the client
  runs a `ReverseProxy` and the engine points `BaseURL` at loopback.
  Rejected: `dagger call` in CI and headless/remote engines have no client to
  proxy through, so you maintain two credential architectures forever; and it
  puts an extra hop on the SSE streaming hot path to harden one token when
  the engine already holds every registry credential and `env://` secret.
  Revisit only for a shared/hosted engine serving multiple users'
  subscriptions.
- **Move refresh engine-side.** Rejected: the refresh token is the
  longer-lived, higher-value credential, and it is single-use and rotating —
  if the engine refreshes, the client's `config.toml` holds a dead token and
  the next `dagger` invocation logs the user out. The good part (a proper
  expiry-aware `TokenSource`) is step 3, without moving the refresh token.

## Invariants to preserve

- Plain API keys and CI must be **exactly** unaffected:
  `newCredentialTransport` returns the base transport unchanged when there is
  no source. There is a test for this; keep it.
- Nothing credential-related may enter a dagql ID or content digest. Replay
  (`routeReplayModel`) has no credential and must stay that way.
- `loadLLMRouter` seeds from the main client first so a nested `dagger agent`
  never holds credentials — the reloader must resolve against **the same**
  client that supplied the value. The staged spike does this via `bindClient`
  in `LoadClientConfig`; do not regress it.
- Bearer tokens must not reach telemetry. `llmOTelTransport` logs bodies, not
  headers — keep it that way.

## Verification

```
go build ./core/... ./engine/... ./internal/cmd/...
go test ./core/ -run 'TestLLM|TestAnthropic|TestCodex|TestCredential' -count=1
go test ./internal/cmd/dagger/llmconfig/ ./internal/cmd/dagger/ -count=1
```

`-race` could not be run in the audit sandbox (`CGO_ENABLED=0`, no C
compiler). Run it if your environment allows — specifically against the
`Clone()`/`Endpoint()` race noted above. The `os.Setenv` concern turned out
*not* to be a data race (`os.Setenv`/`LookupEnv` are serialized by
`syscall.envLock`); it is a lost-update/TOCTOU between the hook's `Setenv`
and `applyLLMConfigEnv`'s `LookupEnv`-then-`Setenv`.

End-to-end, the thing to actually confirm: a session that outlives its access
token keeps working, and a token refreshed by one agent is observed by all
the others.
