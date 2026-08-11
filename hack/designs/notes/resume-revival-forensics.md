# Resume revival: forensics on the 33-agent blowup

Two saved sessions: `testdata/before.json` (3 workers, pre-restart) and
`testdata/after.json` (post-restart, after "check on them"). Item 13 of
`hack/designs/async-agents.md` §10 already establishes the mechanism (receiver
load re-executes recorded `Staff.spawn`); this note only supplies the numbers
and corrects two details item 13 gets wrong.

## Verdict, first

**33 = 3 × 11.** Three is MEASURED: the restored toolset binding carries
exactly three `Staff.spawn` frames (`scout1/2/3`), digest-for-digest the same
three `before.json` recorded. Eleven is the number of times that binding was
re-hydrated while it still pointed at the *pre-restart* chain. Nothing else in
the files multiplies: not the 10 distinct spawn nodes, not the 2 `withTools`
selectors that embed a staff chain, not dump-id's 19 EXPANSIONS.

Eleven is measurable two ways, and the save cannot tell them apart (§5):

- **per step** — 11 post-resume `withResponse` batches contain ≥1 `staff_*`
  tool call (spine 17, 19, 23, 27, 29, 33, 54, 59, 64, 69, 77) → 11 × 3 = 33
  over the whole session;
- **per tool call** — 27 post-resume staff tool calls; the running total
  crosses 33 at the **11th**, the end of the `staff_logOf` batch at spine 29,
  mid-way through the "check on them" turn (and reaches 42 by the turn's end).

Both land on 33 at a natural observation point. Which unit is right is the one
open question, and §5 says what settles it.

## Reproducing the measurements

```
dumpId(file: "testdata/before.json", stats: true)
dumpId(file: "testdata/after.json",  stats: true)
dumpId(file: "testdata/before.json", find: "spawn", lit: 200)
dumpId(file: "testdata/after.json",  find: "^Staff\\.spawn$", lit: 120, spine: 2)
dumpId(file: "testdata/after.json",  find: "^LLM\\.agent$", lit: 30, spine: 0)
dumpId(file: "testdata/after.json",  find: "LLM\\.withTools", lit: 60, spine: 1)
dumpId(file: "testdata/after.json",  find: "^Query\\.(host|currentWorkspace)$|withMountedFile", lit: 30, spine: 2)
dumpId(file: "testdata/after.json",  find: "^Query\\.llm$", lit: 30, spine: 1)
dumpId(file: "testdata/after.json",  tree: true, depth: 1, lit: 40, limit: 200)
dumpId(file: "testdata/before.json", diff: "testdata/after.json", lit: 40, limit: 70)
```

Vocabulary, from `cmd/dump-id/graph.go`: **DISTINCT** = call nodes in the flat
`callsByDigest` map. **REFS** = in-edges with multiplicity (receiver, module,
`arg:*`, `implicit:*`). **EXPANSIONS** = distinct root→node paths, i.e. what a
walker *without* digest dedupe would hit (`graph.paths`, graph.go:89-115).
EXPANSIONS is **not** an execution count — see §3.

## 1. Spawn inventory

`before.json`: 3 distinct `Staff.spawn`, one linear chain, each EXPANSIONS 1.

```
staff.spawn(scout1) xxh3:1862d7db34321989  cachePerCall 9781z4aoyfgku5l4czfv2gqrk
  .spawn(scout2)    xxh3:8a83faf82b8a3bf7  cachePerCall rkxg6lh7xlewxk0pp0pj8jlof
  .spawn(scout3)    xxh3:6322c5be21326740  cachePerCall bg1wvqfg2i566ly7vtp5228ug
```
all with `chief: LLM.agent xxh3:842b810007636914`, `source: Query.currentWorkspace
xxh3:3da1170ceb43dd0d`; the tip is `arg:object` of `LLM.withTools xxh3:53a21a9eb550c5a…`.

`after.json`: `Staff.spawn DISTINCT 10, REFS 10, AS-RECV 9, EXPANSIONS 19`.
Two chains, both rooted at the *same* `Query.staff xxh3:6ac7dc605ce5fb29`:

**Chain A — the originals, replayed verbatim (3).** `xxh3:1862d7db34321989`,
`xxh3:8a83faf82b8a3bf7`, `xxh3:6322c5be21326740` — byte-identical digests to
`before.json`, recorded per-call nonces included. EXPANSIONS 4 each. Tip is
`arg:object` of `LLM.withTools xxh3:c4de6f4963abe584`, the resume-time binding.

**Chain B — re-derived + genuinely new (7).**
```
staff.spawn(scout1)     xxh3:2be6c5fcf41d3507   chief 210687b1  cachePerCall a3zjqo2f…
     .spawn(scout2)     xxh3:546729cd1b06ea3d   chief 210687b1  cachePerCall 4cmmj937…
     .spawn(scout3)     xxh3:fbce94168fcec9ec   chief 210687b1  cachePerCall rgbx00ep…
     .spawn(forensics)  xxh3:c546b7f9bfa702bc   chief 8a9fca8b
     .spawn(engine)     xxh3:2e55ae2866bb22d0   chief 8a9fca8b
     .spawn(persistence)xxh3:1c8d3946b3ff622f   chief 8a9fca8b
     .spawn(apipattern) xxh3:7f758329fe975233   chief 8a9fca8b
     .dismiss(scout1).dismiss(scout2).dismiss(scout3)
```
all with `source: Workspace.withMountedFile xxh3:64464e58b599b2f1`. EXPANSIONS 1
each. The last four are REAL new hires: `staff_spawn` tool calls appear in the
recipe at spine 54 (callIds `toolu_01M9Nzxo…`, `01PFAryJ…`, `01TGv1uh…`,
`01W3SdyR…`). The first three were never re-requested by the model — same names,
same task strings as chain A — they are the *executed* form of chain A.

**Why the replayed three have different digests.** Two arguments moved:

- `source:` `Query.currentWorkspace xxh3:3da1170ceb43dd0d` →
  `Workspace.withMountedFile xxh3:64464e58b599b2f1`. `currentWorkspace` prefers
  a workspace bound in context over the session's own
  (`core/schema/workspace.go:606-614`), so re-evaluating it inside the chief's
  tool-call context returns the chief's *live* overlaid workspace — by then
  `currentWorkspace xxh3:83d5d22525a91797` plus two `.refs/…` mounts.
- `chief:` `LLM.agent xxh3:842b810007636914` → `xxh3:210687b164f2cc96`. Same
  instance-ID literal (`x3c0tfmoo5hphht0ung67wv48`), same eight selectors; the
  only difference is selector 2, `withWorkspace(workspace: currentWorkspace
  xxh3:3da1170…)` → `withWorkspace(workspace: withMountedFile xxh3:64464e58…)`.
  Same substitution, one level down.
- Consequently a fresh `cachePerCall` nonce per frame (re-execution re-mints it;
  the recorded one is *not* carried into the re-executed frame — MEASURED, the
  chain B nonces differ from chain A's).

`Query.staff` itself re-derives to the identical digest, so the divergence
begins exactly at the first frame that takes `currentWorkspace`.

`dumpId(file: "before.json", diff: "after.json")` reports
`root spine: 15 -> 80 selectors, identical prefix of 0` even though the two
conversations' first 15 selectors are the same conversation. The divergence is
at selector **1**, not at the workspace: `Query.llm` carries
`cachePerSession` too (`xxh3:87aab2fed41760f4` = session 1,
`xxh3:a8bf8c8fc6067a95` = session 2), and every digest downstream of a changed
leaf changes. Independent confirmation that a resumed session re-executes the
seed rather than adopting the recorded one.

The four new workers carry `chief: LLM.agent xxh3:8a9fca8bbb7029ca`, instance
`hiw4b4zpfhnz3h7mijhtkf2cy` — a *different* chief instance, i.e. the resumed
session's own conversation.

## 2. Where the staff object rides

`arg:object` of `LLM.withTools`. `after.json` has 7 distinct `withTools`; 4 bind
a `Staff`, of which **exactly 2 embed spawn frames**:

- `xxh3:c4de6f4963abe584` → chain A tip (3 frames) — the binding as restored;
- `xxh3:17c165db20142194` → chain B's `dismiss` tip (7 frames) — the binding as
  saved, on the root spine at selector 3.

The other two (`xxh3:40a1c815e054c36a`, `xxh3:ff3e7fe0291d52fc`) bind bare
`Query.staff`; they sit inside the two chiefs' own receiver chains.

**This count does not multiply anything.** `LLM.withTools(object:)` is a
`LazyRef` argument: the recipe loader carries it by reference and never
evaluates it (`dagql/server.go:1553-1556`, `:1628-1634`), and the binding is
loaded only when a tool is actually dispatched on it
(`core/llm_object_tools.go:99-104`, `:119-170`). So loading the conversation
walks zero spawns; a *tool call* walks them.

## 3. The arithmetic

**What does NOT multiply.**

- *EXPANSIONS.* Chain A's frames report 4 each (19 total for `Staff.spawn`).
  That 4 is a DAG artifact: chain A tip hangs off one `withTools`, whose LLM
  spine is the receiver of `LLM.agent xxh3:8a9fca8bbb7029ca`, which is
  referenced 4× as `chief:`. A load dedupes by digest —
  `recipeLoadState.load` keys a future per `id.Digest()`
  (`dagql/server.go:1404-1418`) — so one load walks each frame once, not 4×.
- *The 2 spawn-bearing `withTools` selectors.* LazyRef, per §2.
- *The 10 distinct spawn nodes.* That is what got written down, not what ran.
- *Chain growth.* The chain reaches 7 spawn frames, but only the three
  cross-session frames are re-execution candidates (below), so growing sums
  (3+3+3+4+5+6+7 = 31, 3+3+3+3+4+5+6+7 = 34) are ruled out — they would require
  the four new workers to re-execute too, and their leaves carry *this*
  session's stamp.

**What DOES multiply: re-hydrations × cross-session spawn frames.**

- MEASURED (frames = 3): the binding through spine 54 is chain A tip. Proof is
  `LLM.agent xxh3:8a9fca8bbb7029ca` — the chief captured *at* the post-resume
  `staff_spawn` calls — whose spine still reads
  `withTools(object: Staff.spawn xxh3:6322c5be…)` with no later `withTools`
  appended. Read-only staff tools therefore never rebound the binding; every
  read from spine 17 to spine 54 dispatched against the recorded three.
- MEASURED (it is cross-session): `Query.llm` carries `cachePerSession`.
  Session 1 = `qj031ovcqzrwgtmvdrwlcnnix`, session 2 = `j8v8xkpdzir5v3vd6y3nw43or`.
  Chain A's `source` leaf `Query.currentWorkspace xxh3:3da1170ceb43dd0d` carries
  `cachePerSession: "qj031ovcqzrwgtmvdrwlcnnix"` — session 1's.
- CODE: `currentWorkspace` is `NotReplayable` with `PerCallInput` and
  `PerSessionInput` (`core/schema/workspace.go:35-40`).
  `fieldNotReplayable` returns `recorded != state.sessionID`
  (`dagql/server.go:1506-1511`) → true here. The taint propagates *upward* to
  every dependent (`dagql/server.go:1439-1467`), so all three spawn frames skip
  both cache lookups (`:1532-1539`, `:1602-1608`) and go to
  `baseObj.Select` (`:1609`).
- CODE: `@cache(policy: Never)` → `dagql.PerCallInput`
  (`core/modfunc.go:130-134`), so each re-execution is a genuinely new call;
  `LLM.spawn` mints a fresh `InstanceID` per call (§10 item on spawned instance
  identity), so each re-execution is a new roster entry.
- CODE (re-hydrations = one per dispatch, not one per session): the loaded
  object is memoized onto `m.boundTools[i].object`
  (`core/llm_object_tools.go:157-168`), but `step()` opens with
  `llm = llm.Clone()` and the comment names that MCP "**this step's transient
  MCP clone**" (`core/llm.go:1591-1600`). The memo is thrown away at step end
  unless the binding was *rebound*, which is re-emitted as a `withTools`
  selector (`core/llm.go:1721-1745`). A read-only tool never rebinds → the next
  step re-loads.

**The count.** Staff tool calls after `withPrompt("check on them")` (spine 16),
read straight off the recipe's `withResponse` selectors — `spine[n]: tools (n)`:

```
17: status (1)      27: status (1)      54: spawn ×4 (4)    69: sendTo, status (2)
19: read ×3 (3)     29: logOf ×3 (3)    59: dismiss ×3 (3)  77: sendTo ×3 (3)
23: collect ×3 (3)  33: diffOf ×3 (3)   64: status (1)
```

27 calls in 11 steps. Per step: 11 × 3 = **33**. Per call: the 11th call is the
last `staff_logOf` at step 29 — cumulative **33**; the "check on them" turn ends
at 14 calls, i.e. 42.

Batching decides which unit is right, and the code says *both* units exist.
`CallBatch` runs bound-type-returning ("destructive") tools sequentially
(`core/mcp.go:1237-1250`) and everything else in parallel (`:1263-1266`,
`:1573-1592`); `ReadOnly` is "returns something other than the bound type"
(`core/llm_object_tools.go:441-444`), so `spawn`/`dismiss` (→ `Staff!`) are
sequential and share one hydration, while `status`/`read`/`collect`/`logOf`/
`diffOf`/`sendTo` (→ `String`) run in parallel and each goroutine reaches
`srv.Load` before any of them can plant the memo — one hydration each.

## 4. Why digest dedupe does not save it

The recorded nonce **is** replayed byte-identically: chain A's three digests in
`after.json` equal `before.json`'s, `cachePerCall` literals and all. So a
digest-keyed dedupe *would* stop the repeat — and within one load it does
(`state.loads`, `dagql/server.go:1404-1418`). Two things defeat it across loads:

1. `NotReplayable` skips the digest lookup entirely when the recorded session
   stamp differs (`dagql/server.go:1532-1539` guarded by `:1506-1511`). The
   check runs *before* inputs are loaded, which is why the taint has to
   propagate upward at all (`:1424-1428`).
2. Even without (1), chain A's digests are never *populated*. The re-execution's
   frame is rebuilt from the **loaded** inputs
   (`loadedResultCallFromRecipeID`, `dagql/server.go:1592-1609`), so the result
   is cached under chain B's digest, not chain A's. Chain B existing in the save
   at all is the evidence.

**Two corrections to item 13.** (a) It says the replayed call ID is
"byte-identical … so its `dagger.io/dag.digest` is too", which is why revived
loops render under the original `spawn()` row. The *recorded* ID is identical;
the *executed* one is not — it is chain B's digest. Revived loops should
therefore no longer collapse onto the original row, and item 13's rendering
claim needs re-checking against a current engine. (b) Item 13 says the fix
half-landed via `InstanceID` keying; note that `LLM.agent xxh3:842b8100…` and
`xxh3:210687b164f2cc96` carry the *same* instance literal
`x3c0tfmoo5hphht0ung67wv48` and differ only in composition — so instance
identity survives the re-derivation intact, and only the per-session registry
table stands between this and a clean reattach.

## 5. What is not settled

- **Per-step vs per-tool-call hydration.** The save records the binding only at
  resume and at save, so it cannot distinguish 11 steps from 11 calls.
  Settles it: count distinct `dagger.io/agent.id` values per step in the trace
  of a repro, or instrument `recipeLoadState.loadRecipeVertex` with a counter
  and load `after.json`'s `withTools` object twice.
- **Whether the revival stops at spine 54.** The first `staff_spawn` there
  rebinds to the re-derived, current-session chain B, and `step()` persists that
  (`core/llm.go:1725-1745`). Chain B's leaves all carry session 2's stamp —
  `currentWorkspace xxh3:83d5d22525a91797`, and `Host.directory` is likewise
  `NotReplayable` + `PerSessionInput` (`core/schema/host.go:48-49`) — so loads
  of it are replayable and cache-served, which would *cap* revivals at 7 steps
  × 3 = 21 (per-step) or 15 loads × 3 = 45 (per-call). Under the per-call
  reading the running total passes exactly 33 while chain A is still bound; the
  per-step reading needs the post-rebind loads to keep re-executing the three
  scout frames to reach 33. Same experiment settles both.
- **Blast radius per resume is not 3.** Whichever unit wins, the cost is linear
  in staff tool traffic, not a one-off. §10.1's hazard ("do not call a staff
  tool to just check") is understated: it is not one extra round per resume, it
  is one per look.
