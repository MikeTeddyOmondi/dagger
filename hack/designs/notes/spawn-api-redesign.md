# Redesigning the spawn surface so a handle cannot create an agent

Working note for hack/designs/async-agents.md item 13 (§10, "a session restart
silently RE-ANIMATES workers"). The brief: *"the fix might be to have `.spawn`
return an object whose ID refers to the agent in a way that doesn't force an
agent to be created. There's a pretty strong established pattern for this
already."* This note names that pattern, shows why the pinning §8 already
ratified does not deliver it, and proposes three schema shapes with a
recommendation. Evidence is `testdata/{before,after}.json`, decoded with
`cmd/dump-id`.

## 1. The established pattern, and the two properties it turns on

The pattern is **mint inside the effect, address through a pure lookup, return
the pin**: an imperative field generates identity in its resolver, re-executes
a *pure lookup field* keyed on that identity via a real `Select`, and returns
that lookup's ID rather than its own result. The codebase does it three times,
each cited by §8:

- `step()` materializes conversation state as honest `withResponse` /
  `withToolResult` selectors (core/llm.go:1583), not as an opaque result.
- `Agent.send` enqueues, then Selects `Agent.message(id:)` and returns *that*
  chain (core/schema/agent.go:229-249). `message(id:)` reads the registry and
  errors on a miss — it never creates (core/agent.go:360-373).
- `LLM.spawn` mints `identity.NewID()`, Selects `LLM.agent(id:, name:)` and
  returns that chain (core/schema/llm.go:499-544).

Supporting cast, identical in all three: imperative verbs return
`ID! @expectedType` (`Service.start`/`stop`, core/schema/service.go:123-142;
the agent verbs, §8), so a lazy client forces the effect once at the call site
and re-hydrates through `node(id:)` (dagql/server.go:177-212) — replaying the
*lookup*, not the effect.

**A selector is pure-and-non-creating** iff its resolver is a function of its
receiver and arguments and touches no mutable registry except by reading. In
the agent surface exactly four qualify: `LLM.agent(id:, name:)` (builds a value
from two literals, core/schema/llm.go:550-559), `Agent.message(id:)`,
`Agent.state` and `Agent.snapshot` — the last two through `AgentRuntimes.Get`,
which "never creates an entry" (core/agent.go:257-269). Everything else
creates, including the parts that look like reads or shutdowns: `interrupt`,
`pause`, `resume`, `waitFor` and `stop` all route through `GetOrCreate`
(core/schema/agent.go:262-361), which mints a runtime entry *from the value's
`Seed`* (core/agent.go:271-303).

**An ID is replay-safe** iff loading it is inert: every frame in it — the whole
receiver spine, plus every ID-valued argument — is pure-and-non-creating. This
is a property of the *chain*, not of the tip. Loading is `Server.LoadType`
re-Selecting each frame not already in the session's result cache
(dagql/server.go:1338, :1530-1539); an imperative frame gets *executed*.

`Service` cuts both ways. `Container.asService` is pure
(core/schema/service.go:24) and `Service.start` is the effect, so a `Service`
ID is replay-safe by construction — persist it, load it, nothing runs. But the
analogy breaks where §8 says: a `ServiceKey` is a *reusable composition digest*
(core/services.go:157-163), so re-deriving the composition legitimately
re-addresses the same service, while an agent's key is a minted instance ID and
§8 renounced attach-by-rederivation. We want Service's *ID discipline* without
Service's *keying*.

## 2. Why the pinning already in the tree does not deliver it

§8 promises that for `spawn` "the returned ID is the pinned lookup chain, so
re-hydrating it replays the lookup, not the mint". That holds. It is also not
the thing that gets replayed.

**The pinned ID is replay-safe; nothing durable holds it.** `spawn` returns
`llm(…)!withWorkspace(…)!…!agent(id:"x3c0tf…", name:"interactive")` — every
frame pure. `testdata/after.json` confirms it: three `LLM.agent` calls, all as
*arguments* (`arg:chief` of `Staff.spawn`), never as receivers. Those chief
handles are inert on replay and always were.

**What is durable is the module object's chain, and the effect sits in its
receiver spine.** `AutoSaveSession` persists `LLM.portableID`, and
`recipeSelectors` (core/llm.go:2406-2468) emits one `withTools(object: <ID>)`
per bound object. In `testdata/after.json` that argument decodes to:

```
LLM.withTools(object: <ID Staff.spawn xxh3:6322c5be…>, except: [...])
  path: staff.spawn(chief: …, name: "scout1", task: …)
             .spawn(chief: …, name: "scout2", task: …)
             .spawn(chief: …, name: "scout3", task: …)
```

The chief's conversation records no `Agent.send` and no `LLM.spawn` — the
worker's core-level spawn happens *inside* the module call and never reaches
the recipe. What reaches the recipe is `Staff.spawn`, as stacked receiver
frames. §10's own sentence is the whole bug: *"Pinning makes re-hydrating a
result ID inert; it says nothing about re-executing a recorded call."* The
distinction is receiver-spine versus tip: pinning governs what a verb
*returns*, and nothing about a verb some other chain *contains*.

**Why a read triggers it.** `withTools`'s `object` argument is `LazyRef()`
(core/schema/llm.go:130-140), so restoring the conversation does not load it —
but `MCP.boundToolObject` loads it on the first tool call
(core/llm_object_tools.go:119-151), re-hydrating that chain input by input,
which is the error shape item 13 quotes (`load bound object of type "Staff":
load xxh3:…: inputs: …`). Revival lands one level *below* the field, so
`status` — built out of `Get`-never-creates reads
(modules/staff/main.dang:173-181) — revives just as hard as `sendTo`.

**Why it compounds instead of deduping.** `Staff.spawn` carries
`@cache(policy: Never)` (modules/staff/main.dang:84), and core/modfunc.go:132-134
gives such a function `dagql.PerCallInput`, so dagql cannot dedupe it: every
load of the bound object re-runs every spawn frame in the chain. Each re-run
calls `LLM.spawn` afresh (a new `identity.NewID()`), then `worker.send(task)` —
signal-with-start (core/agent.go:330) — so each is a live loop from the seed.
Three workers, eleven tool calls, thirty-three agents. `testdata/after.json`
holds 10 distinct `Staff.spawn` frames across 7 distinct `withTools` bindings
(19 nodes on a naive walk); `testdata/before.json` holds 3 and 4.

**And the module state cannot help.** `members: Map[Agent!]!`
(modules/staff/main.dang:28) is Dang private state: it serializes into the
object's value, while the object's *ID* is the chain of calls that produced it.
The worker instance IDs live only inside that value; the durable artifact knows
only how to re-derive them. Stated sharply: **the roster is persisted as a
recipe for hiring, not as a list of who was hired.**

## 3. Three shapes

Each answers the same three questions: the schema, what a resumed session does,
and what a dead ID resolves to.

### Shape A — `AgentSpec`: the literal `asService` → `start` split

```graphql
type LLM {
  """This conversation packaged as a startable agent definition. Pure."""
  asAgentSpec(name: String): AgentSpec!
}

type AgentSpec implements Node {
  id: ID!
  name: String!
  seed: LLM!
  """Mint a fresh instance from this spec and register its runtime."""
  spawn: ID! @expectedType(name: "Agent")
}
```

*Resumed session:* an `AgentSpec` ID replays inertly — but only the spec. A
chain that recorded `spec.spawn` still re-mints, so a module holding
`[AgentSpec!]` holds templates rather than workers, and one holding `[Agent!]`
is no better off than today. *Dead ID:* not applicable; a spec has no runtime.

*Rejected, and recorded because it is the tempting one.* The only version that
fixes anything is the full Service analogy — key the runtime on the spec's
digest, so `spec.start` is idempotent and the spec alone is the address. That is
`asAgent(name)` plus digest keying, which §8 ratified away after live QA
(dismiss-and-rehire addressed the predecessor's tombstone) and §10.2 again
(Mode B: a re-derived `currentWorkspace` changes the digest, so the handle
addressed nothing, then *manufactured* a second loop). Keep minted instances
and Shape A collapses into Shape B plus a type.

### Shape B — the handle is a pure reference; the seed lives in the runtime

The instance ID is the whole address, and the lookup is rooted at `Query`, so
the handle's ID carries no composition at all.

```graphql
"""An opaque, engine-minted agent instance identity."""
scalar AgentInstanceID

type Query {
  """
  Rehydrate a handle for a spawned agent instance. Pure: it names an instance
  and never creates one. Null when this session has no instance with that
  identity — the ID is stale (its session ended) or forged.
  """
  agent(instance: AgentInstanceID!): Agent
}

type LLM {
  """
  Spawn this conversation as an agent: mint an instance, register its runtime
  with this conversation as its seed, and return a handle pinned through
  Query.agent. The seed is read here and nowhere else.
  """
  spawn(name: String): ID! @expectedType(name: "Agent")
}

type Agent implements Node {
  id: ID!
  instance: AgentInstanceID!
  name: String!
  state: AgentState!
  snapshot: LLM!
  message(id: String!): AgentMessage!
  # start / send / interrupt / pause / resume / waitFor / stop unchanged:
  # ID! @expectedType, but resolving an existing entry instead of creating one.
}
```

`LLM.agent(id:, name:)` is **deleted**, and with it `Agent.Seed`
(core/agent.go:29-56): the value becomes `{InstanceID, Name}`, a pure
reference. `spawn` registers the entry eagerly rather than leaving it to lazy
creation, so `AgentRuntimes.GetOrCreate` (core/agent.go:271-303) is **deleted
too** — every verb resolves an existing entry or errors.

*Resumed session:* the recorded handle is `Query.agent(instance: "…")` — two
frames, both pure, no leaves. Loading it yields `null` (the previous session's
registry is gone), so the module sees an absent worker: no revival, no
duplicate loop, no re-derived workspace.

*Dead ID:* `null`. Within a session a stopped agent is still an entry (the
tombstone §3.5 requires), so `STOPPED` and "gone" stay distinguishable; across
sessions the honest answer is absence, and it is *loud* — today the same
situation projects `IDLE` with the seed as `snapshot`, which item 12 records as
"not merely empty, it can be plausible and wrong". Callers get a nullable and
must handle it; that is the point.

*Capability model:* §3.3 renounces a *namespace* — `Query.agents`, name lookup,
enumeration. `Query.agent(instance:)` adds none of those; it is a single-key
lookup on unforgeable engine entropy. §10.2 already settled the substance when
it keyed the registry on `InstanceID`: the ID and the composition come out of
the same trace, "so keying on either draws the capability boundary in exactly
the same place: *can you read this session's telemetry*." What changes is the
cost of possession — today the whole composition, after this the 25 characters
already published as `dagger.io/agent.id`; if that needs narrowing, §10.2's
option (b) (authority in another layer, e.g. the CLI's ownership flag) stays
available. The size follows: §8 measured the handle at ~24 bytes and the recipe
form at "~350 bytes for a bare `llm` seed, growing with the composition", and
Shape B makes the recipe form constant-size and leaf-free — what §4.1 needs
("persist the recipe form, not the handle form"), and what kills Mode B's
*class* rather than its instance, since no composition in the chain means no
non-replayable leaf can perturb it.

### Shape C — make module-held agent state replay-safe

B fixes the handle, not `Staff.spawn`: the effect is in the module function,
and the module function is what gets replayed. Same pattern, one level up —
**an effectful module tool returns a value pinned through a pure recorder, so
its own frame never enters a durable ID.** Dang self-calls are real dagql
selections rooted at the module's root binding, so the recorder carries state
as arguments:

```dang
"""One roster slot: a name and the worker instance it addresses."""
type Slot { name: String!  worker: Agent! }

type Staff {
  let members: Map[Agent!]! = [:]

  """Record a roster. Pure: literals and pure agent references only."""
  roster(slots: [Slot!]!): Dagger.Staff! { ... }

  spawn(...): Dagger.Staff! @cache(policy: FunctionCachePolicy.Never) {
    let worker = base.spawn(name: name)   # the effect, once
    worker.send(task)
    staff.roster(slots: slotsOf(members.with(name, worker)))   # the pin
  }
}
```

The chief then records `withTools(object: <ID Query.staff.roster(slots:
[{name: "scout1", worker: <ID Query.agent(instance: "x")>}, …])>)` — pure
frames, literal arguments, replay-inert.

*Resumed session:* the roster rebuilds with the same instance references it
had. In-session they address the live runtimes; cross-session they resolve to
`null` until §4.1's session-independent registry lands, at which point this
shape *becomes* reattachment with no further schema work. *Dead ID:* `null`,
surfaced by the module — `member(name)` can say "worker 'scout1' did not
survive the restart" instead of raising a corpse.

*For every module that holds agents:* state that outlives a call must be
reachable as data in the recorded call, not as a consequence of it. Concretely:
state-bearing tools return a pure recorder self-call; `Map` state must become
an exposed list type (Dang cannot expose maps); and the recorder must genuinely
be pure, since a `@cache(Never)` recorder reintroduces `PerCallInput` and loses
dedupe. `modules/staff` is the only module holding agents today —
`modules/delegate` uses the synchronous `LLM.loop` — so the migration is one
module, until item 5 makes `loop` sugar over the runtime and the rule becomes
load-bearing for every module that runs a loop.

### Shape D, for completeness

Item 13's runner-up: make a `DoNotCache` + `@expectedType` frame in a *loaded*
chain resolve to its recorded result rather than re-execute. The only option
that protects modules which have not adopted C, and the only one that fixes
receiver-load revival for arbitrary third-party modules — but it changes
semantics engine-wide, needs a durable home for recorded results, and inherits
item 13's atomicity finding. Keep it as the safety net under B+C.

## 4. Recommendation

**Shape B, with Shape C as its module-layer corollary.** B is the direct answer
to the brief — `spawn` returns an object whose ID refers to the agent and
cannot construct one — and it is the established pattern with one screw turned:
the pure lookup moves from `LLM.agent(id:, name:)` to `Query.agent(instance:)`,
dropping the seed out of the handle. Deleting `Agent.Seed` and `GetOrCreate`
makes the guarantee structural rather than disciplinary: afterwards *no
selector in the schema can create an agent except `spawn`*, so §10.2's "a miss
is a constructor" hazard is abolished rather than fixed. C is what makes the
fix reach the reported bug, since B alone leaves `Staff.spawn` in the receiver
spine. Sequence B first: it is engine-only and independently testable, and C
depends on it (without B a recorded `Agent` argument still drags a
`currentWorkspace` leaf through every replay).

## 5. Cost

**§8 ratifications.** *"Agent identity is minted at spawn and pinned by
re-exec"* survives in substance but changes lookup: the pin becomes
`Query.agent(instance:)`, so "extending the message-identity pattern one level
up" becomes one level *out*, to the root. Withdrawn with it: `LLM.agent(id:,
name:)` as public API, and "IDLE-from-absence is the honest projection of a
never-started agent" — after B there is no never-started agent, and absence is
`null`. §3.1's `LLM.agent` block and §10's "Spawned instance identity" bullet
need rewriting; §3.3's no-namespace renunciation needs an explicit carve-out
for single-key lookup by minted token (the argument is already written, in
§10.2). *"Imperative verbs are ID-returning"* is untouched and carries more
weight than before. `Agent.instanceID` becomes `Agent.instance` and stops being
a correlation-only field — it is the address.

**Tests.** `core/integration/agent_runtime_test.go`: `TestRosterAddressing`,
`TestRosterAddressingFromModule`, `TestRosterAddressingHostWorkspace` (:1161,
:1245, :1414) keep their assertions but rebuild a two-frame chain, and the
host-workspace case becomes trivially true rather than key-dependent (keep it
as a pin). `TestSpawnInstances` (:513) and `TestSpawnAfterStop` (:596) are
unaffected in intent. Owed: a resumed-session test asserting a recorded roster
mints zero runtimes, and a null-resolution test for a stale instance.
`dang_forcing_test.go`'s `TestLazyChainForcing` (:35) still uses `LLM.spawn` as
its instance-counting witness, but its fixture's re-hydration path changes.
`staff_test.go`'s `TestAskChiefAndCollect` (:71) is already skipped (item 15)
and needs re-recording regardless, since C changes the chain the replay
provider matches.

**Modules.** `modules/staff` takes real work: `members`/`tombstones` become
exposed slot lists, `spawn`/`dismiss` return a `roster(...)` self-call instead
of mutated `self`, and `member(name)` must handle a null worker; its `Agent!`
arguments (`spawn(chief:)`, `chiefLine(boss:)`) get shorter IDs for free.
`modules/delegate` is unaffected today and inherits the rule when item 5 lands.

**The biggest objection.** B trades a *silent* failure for a *loud* one without
yet delivering what the user wants. After it, resuming and asking "check on
them" reports three dead workers instead of hiring thirty-three — correct,
honest, and still not "the workers kept running". Reattachment needs the
session-independent registry item 13 recommends and the lifetime question §4
dodges (when does an agent outlive every session that can see it?). B is a
precondition for that work rather than a substitute — a constant-size,
leaf-free, purely referential handle is exactly what a cross-session registry
must be keyed and addressed by — but it should be proposed as "stop the
bleeding and clear the path", not as the resume story.
