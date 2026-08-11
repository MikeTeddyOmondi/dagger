# Saving and restoring a session's agents

Working notes for hack/designs/async-agents.md §4.1 (resume from trace), item 13
(a restart re-animates workers) and item 1's save-identity thread. Scope: what a
CLI session writes to disk, and what a resume would have to do so the agents that
were running are *reattached or rehydrated* rather than re-spawned.

## 1. Today's path, precisely

**What is written.** One file per session, `$XDG_STATE_HOME/dagger/llm-sessions/<uuidv7>.json`,
mode 0600 in a 0700 dir (`getSessionDir`, internal/cmd/dagger/llm.go:405-426). The
schema is five fields (`sessionMetadata`, llm.go:394-401) and the whole conversation
rides in one of them:

```json
{
  "name": "check on them",
  "model": "claude-opus-5",
  "created_at": "2026-08-10T23:49:36Z",
  "llm_id": "CsiuEgoVeHhoMzoxYmFhNDIzOWRlMGFkNzhjEuUB…"
}
```

`branch` is the fifth field and is dead: nothing sets it (llm.go:464-469); only the
picker reads it (shell_commands.go:359-363). There is no `version` field.

`llm_id` is `LLM.portableID` (llm.go:450 → core/schema/llm.go:168-186 →
`PortableRecipe`, core/llm.go:2551), the **recipe** form: a flat re-Select rooted at
`Query.llm` carrying tool bindings, skills, MCP servers, the tip-most workspace
binding *verbatim including its overlay*, and one selector per message
(`recipeSelectors`, core/llm.go:2406-2546). It must be the recipe: a post-evaluation
`Result.ID()` is an engine-local shared-result reference that dies with its session.

Measured on the two real saves in `testdata/` (`dump-id -stats`):

| | before.json | after.json |
|---|---|---|
| encoded `llm_id` | 23,252 b64 chars / 17,439 proto bytes | 401,168 / 300,876 |
| distinct calls | 37 | 169 |
| root spine | 15 selectors | 80 selectors |
| naive expansion | 90 nodes | 791 nodes |
| `Staff.spawn` | 3 distinct | 10 distinct, 19 expansions |

**When it is written.** Only automatically. `LLMSession.onStep` is wired at LLM init
(shell.go:833-847) and fires after every prompt turn — including an interrupted one —
via `stepped` (llm.go:289-293), and after `ctrl+s` / `ctrl+u` (`ExportChanges` /
`ResetWorkspace`, session_agent.go:820, :843). **There is no `/save` or `.save`
command** (builtins: shell_commands.go:195-335); §5.1's "/save" is an intended
affordance, not a shipped one. `ctrl+s` is *export the workspace overlay to disk*, not
*save the session*; it writes a session file only as a side effect of `stepped`.
Keyed by `h.sessionUUID`, held on the shell handler beside `h.initialPrompt`
(shell.go:136-143), read via `saveIdentity()` (:254-259) and cleared by
`resetSaveIdentity()` on branch/resume (:261-267, shell_commands.go:329, :398).

**What is read back.** `LoadSession` (llm.go:495-540): unmarshal, refuse an empty
`llm_id`, `dagger.Ref[*dagger.LLM](dag, ID(llm_id))`, `Replay` for telemetry, optionally
prepend a conflict-marker cue, then `updateLLM` — which calls `dropAgent()` first
(session_agent.go:616-619), so the resumed conversation has **no** runtime and the next
submit spawns a fresh one (`currentAgent`, :271-296). Entry points: `.resume`
(shell_commands.go:306-334), the picker (:340-400), `--resume` (functions.go:1116-1124).

**Where the agents are lost.** Three distinct losses, worth separating:

1. *The chief's own runtime.* `updateLLM` drops it and the next prompt `spawn`s a new
   instance. Benign — one conversation, new instance ID, no duplicate work.
2. *The workers are not saved at all.* Nothing in the file mentions them. Their names,
   spawn-minted instance IDs, states, transcripts and per-worker workspaces exist only
   inside `AgentRuntimes`, which is allocated per session (engine/server/session.go:117,
   :532) and killed at teardown (`agents.KillAll`, :638-639).
3. *The workers are re-spawned anyway.* They are implied by the chief's recipe. `dump-id`
   on after.json shows `LLM.withTools(object: staff.spawn(…scout1).spawn(…scout2).spawn(…scout3))`
   — three imperative frames nested inside an ID literal argument, each carrying its
   recorded `implicit cachePerCall: "9781z4aoyfgku5l4czfv2gqrk"` nonce. Loading that
   receiver re-executes them (item 13: the trigger is *receiver load*, so even a read
   revives), and because the replayed call ID is byte-identical the new loops render
   under the original `spawn()` row. Item 13's measured result: 3 workers → 33 loops.

**The save-identity wart.** `AutoSaveSession` is a method on **one** `sessionAgent`
(llm.go:437) but the file identity is **session-wide** (`h.sessionUUID`). `onStep`
receives the stepping conversation, so with two conversations the last to step wins
the file, and the file's `name`/`model` describe whichever that was. Once a roster
exists this is not an edge case: attaching to a worker (llm.go:235-262) makes it a
peer conversation that steps on its own. The file has no room to say "there were
three of us", so the fix and the agents fix are the same fix — the artifact has to
stop being one conversation.

## 2. Options

Common ground: the durable identity is the spawn-minted `InstanceID` (core/agent.go:34-50),
the registry keys on it (`agentKey`, :249-255), and — load-bearing for every option
below — **the seed is read off the value only when the entry is CREATED**
(`GetOrCreate`, :279-303). A handle naming an existing instance addresses the entry as
it stands; a handle naming an absent one defines it.

### A — richer save file: `agents[]`, cold rehydration from the transcript

```json
{
  "version": 2,
  "created_at": "2026-08-10T23:49:36Z",
  "focus": "x3c0tfmoo5hphht0ung67wv48",
  "agents": [
    {
      "instance_id": "x3c0tfmoo5hphht0ung67wv48",
      "name": "interactive", "role": "session", "state": "IDLE",
      "model": "claude-opus-5", "title": "check on them",
      "generation": 3,
      "llm_id": "CsiuEgoVeHhoMz…",
      "degraded_bindings": [{ "type": "Staff", "reason": "imperative", "recipe": "CqoB…" }]
    },
    {
      "instance_id": "m39gowtw3zfw4e71g5ta490jp",
      "name": "scout1", "role": "worker", "state": "RUNNING",
      "spawned_by": "x3c0tfmoo5hphht0ung67wv48", "generation": 1,
      "llm_id": "Cq8BChV4eGgzOj…"
    }
  ]
}
```

Widening `sessionMetadata` (llm.go:394-401) into `{version, created_at, focus, agents}`,
where each entry is:

```go
type savedAgent struct {
	InstanceID string           `json:"instance_id"` // registry key; reused on restore
	Name       string           `json:"name"`        // display only
	Role       string           `json:"role"`        // session | worker | attached
	State      string           `json:"state"`       // advisory; recomputed from the runtime
	SpawnedBy  string           `json:"spawned_by,omitempty"`
	Generation int              `json:"generation"` // bumped on every restore of this instance
	LLMID      string           `json:"llm_id"`     // recipe of the LAST COMMITTED snapshot
	Degraded   []droppedBinding `json:"degraded_bindings,omitempty"`
}
```

Engine-side: nothing new is strictly required, which is the surprise. Resume is
(1) decode each `llm_id`; (2) `LLM.agent(id: instance_id, name: name)` — the pure
lookup spawn pins through (core/schema/llm.go:218-224, :550-559), which never mints;
(3) the first `send`/`start` creates a registry entry whose seed is *the saved
transcript*, so the agent continues with its history instead of from its opening task;
(4) the client binds it with `bindRuntime` (session_agent.go:309-316), `owned` per
`role`. The roster rejoins the same entry because it keys on the same ID.

Engine restarted / agent gone: the option that does not care — the entry's absence *is*
the normal case, and the file is the source of truth.

Workspace: each worker's recipe carries `withWorkspace` verbatim including its overlay
(core/llm.go:2470-2493), so its touched-path changeset — full content per touched path,
§8.1 — is inlined into the file. The base is `Query.currentWorkspace`, which is
`NotReplayable` + `PerCallInput` + `PerSessionInput` (core/schema/workspace.go:35-37),
so it re-detects the *resuming* client's workspace: correct for the same checkout, a
lie for a different one. Size: the chief's recipe here is already 401 KB of base64 with
zero worker WIP; N workers with real edits inline their content N times.

Failure modes: (a) it does **not** by itself stop duplication — the chief's recipe still
contains the `Staff.spawn` frames, so this needs the save-time refusal in §5; (b)
restoring reuses a minted instance ID, weakening "uniqueness is minted where instances
are born" (§8) — if the old engine still runs, two live runtimes share one ID in two
registries and a third client's roster merges them (hence `generation`); (c) only the
last committed step survives — a mid-turn worker's in-flight tool call and queued
mailbox are lost, the rule §8.1 already states for harvest; (d) file size grows without
bound with worker count and WIP.

Migration: a v1 file has no `version` → read as one agent, `role: "session"`. Forward,
an old binary reading v2 finds no top-level `llm_id` and fails loudly (llm.go:515-517).
That is the right breakage: do NOT mirror the focused conversation into a top-level
`llm_id` for compatibility, or an old binary silently resumes the chief and discards the
workers — the exact failure this note exists to remove.

### B — the trace is the directory (§4.1)

```json
{
  "version": 2,
  "trace": { "id": "4bf92f3577b34da6a3ce929d0e0e4736", "root_span": "00f067aa0ba902b7", "store": "cloud" },
  "focus": "x3c0tfmoo5hphht0ung67wv48",
  "agents": [{ "instance_id": "m39gowtw3zfw4e71g5ta490jp", "name": "scout1" }]
}
```

~200 bytes. Engine-side: nothing; **client**-side a persisted span store the CLI can
read back (Cloud's API, or a local mirror), plus the call-payload log channel that
already ships (§10.2 "Mode A"). Everything else exists: `dagql/dagui/agents.go:23-44`
folds `dagger.io/agent.*` telemetry into `AgentNode{ID, Name, State}` keyed on the
instance ID, `dagql/idtui/agent_roster.go` renders it, and `encodedIDForCallDigest`
(frontend_pretty.go:5737) rebuilds a *sendable* handle from it. Resume: rebuild a
`dagui.DB` from the persisted trace, project `DB.Agents()`, rebuild handles, reattach by
instance ID; never replay `spawn`.

Engine restarted / agent gone: the rebuilt handle resolves and then addresses nothing —
`Get` never creates, so it reads IDLE-with-seed-snapshot. Watch-only, honestly: this
option makes *addressing* durable, not *agents*. Workspaces are not in the artifact at
all, so a cold engine loses them entirely.

Failure modes: an external service backing a local feature; degraded offline resume; a
roster only as complete as the trace the client may read (§3.3's capability model, now
with a network dependency); and it inherits "the trace carries recipes, not snapshots".
Migration: additive; v1 files simply have no `trace`.

### C — engine-side registry that outlives the client session (item 13's fix)

```json
{
  "version": 2,
  "engine": { "id": "…", "endpoint": "unix:///run/dagger.sock" },
  "focus": "x3c0tfmoo5hphht0ung67wv48",
  "agents": [
    { "instance_id": "x3c0tfmoo5hphht0ung67wv48", "name": "interactive" },
    { "instance_id": "m39gowtw3zfw4e71g5ta490jp", "name": "scout1" }
  ]
}
```

~30 bytes per agent. Engine-side: move `AgentRuntimes` off `daggerSession`
(session.go:117, :532) onto the `Server`, drop the unconditional `KillAll` at teardown
(:638-639), give entries an owner + TTL, and add a GC. The registry already keys on
`InstanceID`, so *that half has landed* (§10.2); the session-independence half has not.

Resume: for each entry, `dag.LLM().Agent(id, name)` on a **bare** `llm` receiver, then
`Agent.snapshot` and re-root. That works precisely because the seed is only consulted at
entry creation — with the entry alive, the receiver's composition is irrelevant. Which
is also the sharpest capability statement in the design: **the instance ID alone is the
capability**, §10.2 having already conceded that the digest was never the stronger
secret.

Engine restarted / agent gone: the IDs dangle, with nothing to fall back to unless the
file also carries A's recipes. C and A compose: C for the warm path, A for the cold one.
Workspaces stay in the engine, reachable as today via `member(name).snapshot.workspace`,
for as long as the entry lives — now unbounded, so they become a GC liability rather
than a session-scoped one.

Failure modes: it *forces* the lifetime question (§4) rather than answering it; leaked
agents burn tokens with nobody watching; a stale entry is addressable by anyone who
reads an old file; and enumerating "my agents" wants exactly the `Query.agents` §3.3
renounced, so the on-disk file becomes the capability list.
Migration: additive; v1 resumes exactly as today.

### D — durable per-agent journal (C, made real)

C plus: the engine appends each committed step to a per-instance log under its state
dir (`<state>/agents/<instance>/{meta.json,steps/}`), with the workspace changeset held
by content digest under a lease. The registry becomes a *cache* over durable state, and
`stop`/`dismiss` become durable facts rather than session-local ones — the only answer
to item 13's general finding that replaying imperative verbs is not atomic. The file is
C's.

The primitive already exists in outline: `PersistedObject`/`PersistedObjectDecoder`
reconstruct a result "without replaying the original dagql call chain"
(dagql/cache_persistence_self.go:46-59) — exactly the shape "resume must not re-execute
recorded verbs" needs. It does not reach agents today: `@cache(Never)` module functions
are `PerCallInput` and so never cached, and a `DoNotCache` result is detached with no
addressable ID (core/schema/agent.go:216-218).

Failure modes: a new persistence subsystem with its own versioning, GC and crash
semantics; and storing state the design has so far kept computed (§3.4 — a journal must
record *facts*, never `RUNNING`).

## 3. Comparison

| | A file+recipes | B trace | C engine registry | D journal |
|---|---|---|---|---|
| file size | ~400 KB → MBs | ~200 B | ~30 B/agent | ~30 B/agent |
| engine change | none required | none | move + own the registry | new subsystem |
| survives engine restart | yes | no (watch-only) | no | yes |
| survives machine change | conversations yes, host leaves no | as A | no | no |
| stops duplication by itself | no (needs save-time refusal) | yes | yes | yes |
| worker workspace | inlined, re-based on the resuming host | lost | live in engine | durable by digest |
| mid-turn fidelity | last committed step | live entry | live entry | full |
| forces the lifetime question | no | no | yes | yes |
| rough effort | small–medium | medium (needs a trace store) | medium (semantics-heavy) | large |

## 4. Lifetime

Today the answer is implicit and total: agents die with the session that spawned them
(`KillAll`, engine/server/session.go:638-639). Per option:

- **A** — unchanged: *dies with the spawning session*; durability moves to the artifact,
  and a resume creates a new runtime seeded with the old transcript. No new owner, no
  reaper, no cross-session ACL.
- **B** — unchanged, and the option is honest about it: you can watch an agent whose
  session ended, never revive it.
- **C/D** — *lives until explicitly stopped*, plus an idle TTL. Needs a recorded owner
  per instance, an eviction policy, and a GC for the workspaces those entries pin.

**Reject "lives while any client holds it."** Refcounted bindings are right for Services
(`ServiceKey`, core/services.go:157) because a service exists to serve a request. An
agent exists to keep working while you are *not* attached; refcounting would stop every
worker the moment the last terminal closed, which is the failure this whole design
exists to avoid.

**Ship A's policy: an agent dies with the session that spawned it.** It is the only one
that needs no new owner, and it lets the user-visible promise ("resume and your workers
still have their history") ship without also promising "your workers are still running,
somewhere, spending money". When C lands, the policy becomes *lives until explicitly
stopped, with an idle TTL and a recorded owner* — and A's file is unchanged, because
`agents[]` is a superset of C's.

## 5. What to build first

**The smallest change that stops duplication is a save-time refusal, not a restore-time
fix.** In `recipeSelectors` (core/llm.go:2456-2468), when a bound object's recipe
contains a frame that is `DoNotCache` / `PerCallInput`, do not emit the binding: return
it to the caller as a dropped binding instead. The CLI records it under
`degraded_bindings` and prints one line on resume ("staff tools were not restored: 3
spawns could not be replayed"). ~30 lines plus a test, and it converts silent duplicate
work — 3 workers becoming 33, item 13 — into a visible, correct loss. Every option
above wants those frames unreplayed, so nothing is wasted.

Then, immediately after, A's `agents[]`: losing the binding is only tolerable if the
workers themselves come back. That order matters — shipping `agents[]` first would
restore the workers *and* re-spawn them.

What the first step forecloses:

- **Dropping is lossy unless it records.** Keep the dropped binding's recipe in the file
  (`degraded_bindings[].recipe`) rather than deleting it; otherwise a later, smarter
  loader (C, or module-side lookup fields) can never recover old files.
- **Keying `agents[]` on `instance_id` banks instance ID as the durable identity.** C and
  D want the same key, so there is no conflict — but it forecloses name-as-key, and it
  concedes that a *restored* agent reuses a minted ID. Publish `generation` on the state
  record so two runtimes carrying one ID in different engines stay distinguishable.
- **Recipe-in-file banks "the last committed step is a sufficient restore point."**
  Mid-turn fidelity — an in-flight tool call, a mailbox with queued messages — is out of
  reach for A and B forever, and only D buys it back.
- **It does not foreclose C.** A's file already carries C's fields; the warm path can
  start preferring a live entry over the saved recipe the day the registry outlives the
  session, with no format change.
