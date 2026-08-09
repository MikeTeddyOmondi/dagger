# HANDOFF: harvesting work from async worker agents

Continuation notes. Orientation, in this order:

1. `hack/designs/staff-harvest-plan.md` — the ratified two-part plan for this
   change. **Part 1 (engine) is DONE and committed. Part 2 (modules/staff) is
   NOT — that is the main task below.**
2. `hack/designs/async-agents.md` — the underlying async-agent design (§9
   ratified semantics, §11 status).

## Why this change exists

`modules/staff` lets a chief agent hire async workers, but each worker has its
own `Workspace`: everything it edits or commits died with it. This adds a core
API for replaying commits across workspaces (patch-based, drift-safe, with
provenance), and staff tools on top so a chief can harvest a worker's work.

Design decisions were ratified in conversation and are written up in the plan
file — read it rather than re-deriving. The short version: a pure planner
(`Workspace.commitsFrom`) classifies each candidate commit; a strict apply
(`Workspace.withCommitsFrom`) executes; conflicts are *data*, not errors, but a
commit the caller explicitly asked for is never silently dropped; a candidate
touching a path the receiver has uncommitted edits on is refused (DIRTY) so the
chief's WIP is never swept into a worker-attributed commit.

## State of the branch

**Part 1 — engine: DONE, tested, committed.** All 15 new integration tests pass
against a from-source dev engine, plus the pre-existing
`TestWorkspaceCommit*`/`TestWorkspaceWithCommit*` suites (the `stageCommit`
refactor did not regress them). Shipped:

- `WorkspacePendingCommit.Origin` + `WorkspaceStagedCommit.origin`, with
  additive `omitempty` persistence on both payloads.
- `WorkspaceCommitPick` + `WorkspaceCommitPickStatus`
  (PICKABLE/PICKED/REDUNDANT/CONFLICT) and `WorkspaceCommitPickReason`
  (NONE/CONTENT/DIRTY).
- `WorkspaceRepoContainsCommits` (one mount, `git merge-base --is-ancestor`).
- `Workspace.commitsFrom` / `Workspace.withCommitsFrom` / internal
  `__withReplayedCommit`, all behind one shared `foldCommitsFrom`.
- Codegen (`sdk/go/dagger.gen.go`, `docs/docs-graphql/schema.graphqls`, the
  `.mdx` churn) and a changelog entry.

Four places the implementation deviated from the plan, all deliberate:

1. **Embedded arg structs panic for unexported types.** `dagql.InputSpecs.Decode`
   does `fieldV.Set(...)` on the embedded field and reflect refuses when the
   field name comes from an unexported type. The six commit args are spelled out
   with a `commitArgs()` converter instead.
2. **`reason` must be seeded to `NONE`** — the Go zero value for the enum is
   `""`, which serializes as an empty string on a non-null field.
3. **`WorkspaceCommitPick.changes` is resolved for PICKED candidates too**, or
   the non-null field has no value. The Risk-1 "no overlay recorded" hard error
   now applies only to candidates actually being replayed.
4. **A commit staged on an unmoved receiver keeps the SAME sha** (same parent,
   same tree, deterministic `withCommit`), so the "sha differs" assertion moved
   to the drift test; the fresh-pull test asserts `origin` instead.

**Part 2 — modules/staff harvest tools: NOT DONE.** A delegate wrote them and
the work was lost to the harness bug below. Redo from plan Part 2, which is
complete: five tools (`logOf`, `diffOf`, `pull`, `pullConflicted`,
`pullPending`), their semantics, insertion points, the tombstones change, the
chiefPrompt addition, and a long list of Dang correctness notes **verified live
against a from-source engine** (forcing points, rescue placement, list indexing
nullability, that `dagger -m … functions` does NOT type-check bodies — the gate
is `dagger -m ./modules/staff call status`).

## FIRST: fix the harness bug that ate Part 2

**Symptom.** Once a session has called `editor_reload` (or `install`) even once,
it is poisoned: every subsequent workspace edit re-loads the agent's modules. In
this session that meant a delegate editing `modules/staff/main.dang` against an
engine that does not yet have the Part 1 API — the module failed to declare
(`unresolved type: WorkspaceCommitPickStatus`), which failed the whole
delegateEdits call, and its changeset was discarded.

**Diagnosis.** `Workspace.reloaded` (`core/schema/workspace.go:2474`) is
`WithInput(dagql.PerCallInput)` (install at `:342-345`) and returns
`dagql.NewObjectResultForCurrentCall(ctx, srv, parent.Self().Clone())` (`:2501`).
So the returned workspace's ID *contains the `reloaded` call*, per-call input and
all. Every later edit chains off that ID, so the whole chain permanently carries
a per-call segment and nothing downstream of it can be served from cache — module
loads included.

**Fix (user-ratified):** make `reloaded` not appear in the call chain of its own
result. Do the epoch bump for its side effect, then return the **parent's own
result** (`parent`) rather than minting a new one for the current call, so the
caller's subsequent selections chain off the original workspace ID. Check the
other `PerCallInput`/`DoNotCache` workspace fields for the same shape while you
are in there, and add a regression test that an edit made after `reloaded` yields
an ID with no `reloaded` segment.

Do this **before** resuming Part 2 — otherwise the same class of failure will
keep eating module edits.

## Then: Part 2, and the rest

1. **Implement Part 2** from the plan file. Note the gate: `dagger -m
   ./modules/staff call status` type-checks bodies; `functions` does not. The
   subtlest piece is `pullPending`'s drift re-anchoring (the naive version fails
   in the *common* case, because the worker inherited the chief's pending edits
   at spawn and `withPatch` is a plain `git apply` with no 3-way).
2. **Live QA** the harvest loop end to end: hire a worker, have it edit and
   commit, then `logOf` / `diffOf` / `pull` / `pullPending` from the chief.
   Stateful multi-step agent scenarios need ONE session (the runtime registry is
   per-session) — `dagger shell -c` scripts, not separate queries.
3. **Wrap-up:** delete `hack/designs/staff-harvest-plan.md` and this file, and
   fold the plan's ratified semantics into `hack/designs/async-agents.md`
   (the harvest story belongs there alongside the agent lifecycle).

## Pending changes not authored by this work

`dagger.toml` has an uncommitted `[env.dev.modules.staff]` entry (registering
the staff module in the dev env), left pending deliberately — it is a local
enablement choice, not part of the change.

## Open threads carried forward

1. **Module-call cache staleness recurrence.** After a second `editor_reload`,
   identical-arg staff calls replayed stale results DESPITE `@cache(Never)`.
   Not reproduced in the last QA session (single build+install, so the suspected
   trigger — a SECOND reload — was never exercised). Possibly the same root cause
   as the `reloaded` bug above: check whether the fix makes this thread moot.
2. ~~Worker workspace isolation~~ — **this change closes it** (Part 1 done,
   Part 2 pending).
3. **Prompt leak**: the chief system prompt rides into workers via workspace
   compose. Known, mitigated by `workerPrompt`'s closing paragraph, observed
   effective live. Same class as delegate's documented leak.
4. **`Staff.read` counts SYSTEM entries toward `last`** — small `last` values
   return only boilerplate. Consider filtering the SYSTEM role.
5. **Workers don't know their own staff name** — `name` is display metadata on
   the runtime and never reaches the worker's conversation. Cheap fix:
   interpolate it into `workerPrompt` at spawn. Note the test-recording impact:
   recordings match `workerPrompt` byte-for-byte, so the interpolation must be
   part of the public constant's contract.
6. **Idle notification (chief-side).** The chief only learns a worker went idle
   by polling `status` or blocking in `collect`. Natural fit: on turn-end, push a
   "worker ⟨name⟩ went idle" message onto the chief's queue — the same channel
   `askChief` already uses, so it steers an open turn or wakes the chief; no
   polling, none of `collect`'s deadlock exposure. Could be a spawn opt-in
   (`notifyOnIdle: true`) or engine-level (a watch verb on Agent).
7. **Staff E2E integration test** (still unwritten): new `StaffSuite` in
   `core/integration/staff_test.go`, reusing the `agent_runtime_test.go`
   machinery — `cannedReplayModel` recordings via the LLM API (the replayer
   excludes tool results from history matching), worker recordings matching
   `Staff.workerPrompt` byte-for-byte, staff served by copying from the repo root
   (sibling of `copyTestdataFixture`), askChief de-raced with a slow tool after
   the chief's spawn call. Cheap gates first: `dagger -m /src/modules/staff
   functions`, then `call status`, then `call spawn` (negative check).

## Tooling notes

- `engineLab_start` boots the from-source engine (client container mounts the
  workspace at `/src`); `engineLab_engineTest pkg ./core/integration run
  TestWorkspace/TestWorkspaceCommitsFrom` for the new suite. Keyless model:
  `replay/<base64-of-messages-JSON>`.
- Codegen: `generate` with `["go-client:generate", "docs:references"]`. The
  `.mdx` `sidebar_position` churn is normal.
- `golang:lint-all` is red on `main` for unrelated pre-existing hits
  (`core/services.go`, `core/llm_openai_codex.go`, `core/directory.go`, a `dupl`
  in `core/workspace.go`'s `AttachDependencyResults`). Don't chase them.
- Commit style: scoped commits, one logical change each, no trailers.
- `delegateEdits` returns a *changeset*, not commits — a sub-agent's commits do
  not ride back, so commit its work yourself in the parent session.
