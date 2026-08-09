# PLAN: harvesting work from async worker agents

Working document for the in-flight change. **Delete this file in the wrap-up
commit** (like HANDOFF.md).

Problem: `modules/staff` workers each have their own `Workspace`. Anything they
edit or commit dies with them — the chief has no way to get it back. This adds
a core API for replaying commits across workspaces, and staff tools on top.

Two halves, implementable in parallel (disjoint files):

- **Part 1 — engine** (`core/**`, `sdk/go`, `docs`): provenance + planner +
  strict apply.
- **Part 2 — module** (`modules/staff/main.dang`): the chief-facing harvest
  tools built on Part 1.

Ratified design decisions, for both parts:

- Application is **patch-based**, never whole-file overlay. `Workspace.withChanges`
  is a `ReplaceExisting` copy (`core/directory.go:3239`), so applying a worker
  changeset anchored at spawn time would silently clobber the chief's newer
  edits. Drift-safety comes from `Directory.withPatch`.
- **Plan/apply split.** A pure planner classifies; a strict apply executes.
  Conflicts are an *expected outcome* reported as data, not an error — but a
  commit the caller explicitly asked for is never silently dropped.
- **Dirty-path refusal.** A candidate touching a path the receiver has
  uncommitted edits on is refused, so the chief's WIP is never swept into a
  worker-attributed commit (git cherry-pick's rule for a dirty worktree).
- **Skip-and-continue** is the module's policy, not the engine's: the module
  plans, then applies only the PICKABLE set.

---

# Part 1 — engine

All line numbers against current HEAD.

| File | Change |
|---|---|
| `core/workspace.go` | `WorkspacePendingCommit.Origin` + persistence |
| `core/workspace_staged_commit.go` | `WorkspaceStagedCommit.Origin` (`field:"true"`) + persistence |
| `core/workspace_commit_pick.go` | **NEW** — `WorkspaceCommitPick` + 2 enums |
| `core/workspace_commit.go` | **NEW func** `WorkspaceRepoContainsCommits` |
| `core/schema/workspace_commit.go` | `withCommit` → `stageCommit(…, origin)`; new `withReplayedCommit`; carry `Origin` in `stagedCommitEntry` |
| `core/schema/workspace_commit_from.go` | **NEW** — the shared fold + both resolvers |
| `core/schema/workspace.go` | field installs, object install, enum installs |
| `core/integration/workspace_commit_from_test.go` | **NEW** — tests |
| `.changes/unreleased/Added-*.yaml` | changelog |

## 1. Provenance: `origin`

`WorkspacePendingCommit` (core/workspace.go:216-246) gains, after `SHA`:

```go
	// Origin is the hash of the commit this one was replayed from, when it was
	// pulled out of another workspace by Workspace.withCommitsFrom. Empty for
	// commits authored in this workspace by Workspace.withCommit.
	//
	// It collapses transitively to the root: replaying a commit that already
	// carries an origin records THAT origin, not the immediate source's hash.
	// So a commit pulled A -> B -> C still names the commit A staged, and a
	// later pull straight from A recognises it as already present.
	Origin string
```

Persistence, all additive + `omitempty` (old payloads decode as `""`, which is
exactly right for a locally authored commit):

- `persistedWorkspacePendingCommit` (core/workspace.go:857-869): add
  `Origin string \`json:"origin,omitempty"\``; carry it in the encode loop
  (:1057-1065) and the decode loop (:1141-1149).
- `persistedWorkspaceStagedCommitPayload` (core/workspace_staged_commit.go:68-75):
  same, carried in `EncodePersistedObject` (:82-88) and `DecodePersistedObject`
  (:114-120).
- `AttachDependencyResults`, `Clone`, `WithPendingCommit`: no change (plain string).

`WorkspaceStagedCommit` (core/workspace_staged_commit.go:15-25) gains:

```go
	Origin string `field:"true" name:"origin" doc:"The hash of the commit this one was replayed from, when it was pulled from another workspace; empty when it was authored here."`
```

carried in `stagedCommitEntry`'s literal (core/schema/workspace_commit.go:277-284).

### Setting it: an internal field, not a public arg

Refactor `withCommit` (core/schema/workspace_commit.go:75-199):

```go
// stageCommit is the body of Workspace.withCommit, parameterized by the
// provenance to record. origin is "" for a commit authored here.
func (s *workspaceSchema) stageCommit(ctx, parent, args workspaceWithCommitArgs, origin string) (...)
	// exactly the current body, with Origin: origin added to the
	// core.WorkspacePendingCommit literal at :130

func (s *workspaceSchema) withCommit(...) { return s.stageCommit(ctx, parent, args, "") }

// workspaceReplayCommitArgs is workspaceWithCommitArgs plus the provenance a
// replay records. Origin is deliberately NOT part of workspaceWithCommitArgs:
// selectors() (:27) is what reaches __stagedCommit, so provenance can never
// change the commit that gets staged, its hash, or its cache identity.
type workspaceReplayCommitArgs struct {
	workspaceWithCommitArgs
	Origin string
}

func (s *workspaceSchema) withReplayedCommit(ctx, parent, args workspaceReplayCommitArgs) (...)
	// error if args.Origin == ""; else stageCommit(..., args.Origin)
```

Embedded arg structs are supported (precedent: `searchArgs` embeds
`core.SearchOpts`, core/schema/directory.go:746); if it misbehaves, spell the
six fields out.

**Why a field rather than an in-Go loop:** the fold creates one intermediate
`Workspace` per replayed commit. `dagql.NewObjectResultForCurrentCall`
(dagql/server.go:2251) derives identity from the *current* call, so N in-resolver
constructions would collide on one ID. Every fold step must be a real field
selection.

## 2. New object + enums — `core/workspace_commit_pick.go`

Modelled on `core/workspace_staged_commit.go` and the `DiffStatKind` enum
(core/changeset.go:69-101).

`WorkspaceCommitPickStatus` (`dagql.NewEnum`, `Type/TypeDescription/Decoder/ToLiteral`):

- `PICKABLE` — applies cleanly and would be staged.
- `PICKED` — receiver already has it: in its staged stack, in its git history,
  or it already replayed the same origin.
- `REDUNDANT` — applying it would change nothing; content already present.
- `CONFLICT` — cannot be applied; see reason and conflictPaths.

`WorkspaceCommitPickReason`: `NONE`, `CONTENT` (patch no longer applies),
`DIRTY` (receiver has uncommitted changes on a touched path — refused, git
cherry-pick's rule).

```go
type WorkspaceCommitPick struct {
	SHA, Origin, Message, Date, AuthorName, AuthorEmail string // field:"true", docs mirroring WorkspaceStagedCommit
	Status        WorkspaceCommitPickStatus `field:"true"`
	Reason        WorkspaceCommitPickReason `field:"true"`
	ConflictPaths []string                  `field:"true"`
	Changes       dagql.ObjectResult[*Changeset] `field:"true"` // what the commit folded in, in the source
}
```

Decisions:

- **Include `changes`** — the fold already produces it (it is the patch source),
  so it is free, and it lets the module render a per-commit diffstat.
- **No persistence.** `WorkspaceStagedCommit` implements `PersistedObject` because
  it is returned by the `IsPersistable()` field `__stagedCommitEntry` and is
  reachable from a persisted `Workspace`. A pick is a transient projection
  referenced by nothing persisted. `AttachDependencyResults` **is** implemented
  (mirroring core/workspace_staged_commit.go:44) because it carries a live
  `Changeset` whose snapshots must stay attached.
- **`reason` is a non-null enum with a `NONE` member**, not a nullable field:
  `field:"true"` on an enum yields non-null, pointer-to-enum is not an
  established pattern here, and `NONE` reads well from Dang's `case`.
- **`origin` is verbatim** (empty when authored in the source), matching
  `WorkspaceStagedCommit.origin`. The fold's root identity (`Origin ?: SHA`) is
  a local, not surfaced — one word, one meaning.

## 3. Core helper — "is this commit already in the receiver's history?"

`core/workspace_commit.go`, next to `WorkspaceRepoHeadSHA` (:106-121):

```go
// WorkspaceRepoContainsCommits reports, for each hash, whether the repository
// tree's HEAD already has that commit in its history. Every hash is probed
// inside ONE mount (mounting is the expensive part). An unknown or unreadable
// hash reads as absent — the safe answer: the caller falls through to
// content-level classification rather than silently skipping work.
func WorkspaceRepoContainsCommits(ctx, repoDir dagql.ObjectResult[*Directory], shas []string) (map[string]bool, error)
```

Implemented with `withGitMergeWorkspace` (core/changeset.go:1656) + `runGitEnv`
(:1852), like `WorkspaceRepoHeadSHA`, running
`git merge-base --is-ancestor <sha> HEAD` per hash.

The tree to probe is `s.workspaceCommitBaseRepo(ctx, receiverWS)`
(core/schema/workspace_commit.go:622): the newest staged tree when the receiver
has staged commits — whose history holds both the checkout HEAD *and* the whole
staged stack — else the workspace's own materialized repository. One probe
covers "in the staged stack" and "in git history" at once; the in-memory
SHA/origin sets are a fast path that skips the mount entirely when everything
already resolved.

## 4. The shared fold — `core/schema/workspace_commit_from.go` (NEW)

```go
// workspaceCommitCandidate is one source commit as the fold sees it.
type workspaceCommitCandidate struct {
	index   int                                 // index in the source's staged stack
	commit  core.WorkspacePendingCommit
	root    string                              // commit.Origin, else commit.SHA
	changes dagql.ObjectResult[*core.Changeset] // what the commit folded in, in the source
	touched []string                            // workspace-root-relative paths it affects

	status        core.WorkspaceCommitPickStatus
	reason        core.WorkspaceCommitPickReason
	conflictPaths []string
}

// foldCommitsFrom walks the source's staged commits oldest first and classifies
// each against the receiver, folding state forward: every candidate is judged
// against the workspace that would exist if all prior PICKABLE candidates had
// been applied.
//
// Single implementation behind both commitsFrom (projects candidates, throws
// the workspace away) and withCommitsFrom (raises on CONFLICT, returns the
// workspace). Speculative workspaces are free: ordinary immutable dagql values,
// and because every fold step is a real field selection, an apply that follows
// a plan is served from cache.
func (s *workspaceSchema) foldCommitsFrom(
	ctx context.Context,
	receiver, source dagql.ObjectResult[*core.Workspace],
	shas []string,
) ([]workspaceCommitCandidate, dagql.ObjectResult[*core.Workspace], error)
```

### Step 0 — guards, candidate selection

Refuse a remote git receiver (`SourceGitRef()`, cf. core/schema/workspace_commit.go:88).
`selectSourceCommits(srcCommits, shas)` walks the stack **in stack order**,
keeping everything when `shas` is empty. Otherwise resolve each requested hash:
exact match, then unique prefix of >=7 chars (agents get short hashes from
`modules/history`'s `log` and hand them back). Unknown hash → error; ambiguous
prefix → error naming the matches. **Order always comes from the stack, never
from the argument** — a caller cannot reorder a dependent stack by accident.

### Step 1 — receiver identity sets (in memory)

`staged[SHA]` for the receiver's own staged commits; `origins[Origin]` for the
non-empty origins recorded on them.

### Step 2 — git history probe for the unresolved remainder

For each candidate: `root = commit.Origin ?: commit.SHA`; skip if
`staged[SHA] || origins[SHA] || origins[root]`; else probe `SHA` (and `root`
when different) via `WorkspaceRepoContainsCommits`.

**PICKED** ⇔ `staged[SHA] || origins[SHA] || origins[root] || inHistory[SHA] || inHistory[root]`.

- `staged[SHA]` is the common case — a worker's stack *starts as a copy of the
  chief's*, so pulling re-offers the chief's own commits verbatim.
- `origins[root]` is the transitivity rule (A→B→C then a direct pull from A).
- `inHistory[…]` catches "the chief already saved this one and reloaded, but the
  worker still carries it".

### Step 3 — the receiver's dirty paths, computed ONCE

```go
func (s *workspaceSchema) workspaceDirtyPaths(ctx, ws) ([]string, error)
	// changesetTouchedPaths (core/schema/workspace.go:2669) of BOTH
	// ws.git.uncommitted (:2913) and ws.git.unmanaged (:2971)
```

`uncommitted` is the right notion — it is exactly what `withCommit` would sweep
in. `unmanaged` is added because those edits (gitignored / nested repo) are
invisible to `uncommitted` yet would be clobbered by a whole-file overlay, and
`withCommit` refuses to commit them anyway (:506-511). The two sets are disjoint
by construction (`workspaceUnmanagedRemainder`, :3021) and `unmanaged`
short-circuits to empty off the host path (:2982), so this is cheap.

**Invariance (load-bearing):** the dirty set does not change across the fold,
because each applied commit is staged immediately with a scope equal to exactly
the paths it wrote, so the uncommitted remainder never grows. If a future step
ever applies without committing, revisit this.

### Step 4 — the fold

```
cur := receiver
for each candidate c:
  if picked(c): status = PICKED; continue
  c.changes = s.stagedCommitChanges(ctx, srcCommits, c.index)   // core/schema/workspace_commit.go:294
  if srcCommits[c.index].Committed.Self() == nil: hard error (see Risk 1)
  c.touched = changesetTouchedPaths(ctx, c.changes.Self())
  if conflicts := overlappingPaths(c.touched, dirty); len(conflicts) > 0:
      status, reason, conflictPaths = CONFLICT, DIRTY, conflicts; continue
  next, status, reason, paths := s.replayCommit(ctx, cur, c)    // hard errors propagate
  if status == PICKABLE: cur = next
return candidates, cur
```

**The cascade property falls out for free:** a skipped candidate never folds, so
a later commit building on it is patched against a tree lacking its pre-image
and fails → `CONFLICT`/`CONTENT`. Intended; do not special-case it.

### `replayCommit` — patch, classify, stage

1. **Target tree** — the receiver's *current* content, sparse:
   `s.resolveRootfs(ctx, cur.Self(), ".", core.CopyFilter{Include: workspacePathIncludes(c.touched)}, false)`
   (core/schema/workspace.go:792; resolves host+overlay via `resolveHostOverlayRootfs`
   :944 for host-backed, the overlay root for value workspaces).
   `workspacePathIncludes` builds `{p, p+"/**"}` per path with the trailing slash
   trimmed, mirroring `sparseHostBase` (:2640-2644).

2. **Forward patch probe** — `c.changes` → `asPatch` → apply to target with
   `withPatchFile(patch:, onConflict: FAIL)`, then `changes(from: target)`, then
   `delta.Self().ComputePaths(ctx)`.
   `ComputePaths` (core/changeset.go:160) both **forces** the lazy apply and
   yields emptiness + the applied path set in one memoized pass — cheaper than
   `IsEmpty` (:564), which re-walks the trees, and the memo is reused downstream
   by `withChanges`. Wrap in `enginetel.Task(ctx, "apply patch", …)` so a
   *planned* conflict reads as a probe rather than a broken step.

3. **Classify:**
   - no error, empty paths → **REDUNDANT** (patch was a no-op)
   - no error, non-empty → **PICKABLE**, fold (step 4 below)
   - error → **reverse probe**: build the reverse changeset
     (`c.changes.Before.changes(from: c.changes.After)` — Before/After swapped)
     and try to apply it to the *same* target. If it applies → **REDUNDANT**;
     else → **CONFLICT**/`CONTENT` with `parsePatchConflictPaths(err, c.touched)`.

     **The reverse probe is load-bearing, not an optimisation.** `git apply`
     refuses an already-applied patch ("patch does not apply", "already exists in
     working directory") rather than producing an identical tree, so the "chief
     hand-merged the same fix" case would otherwise be misreported as a content
     conflict and the plan would be wrong. A patch whose reverse applies cleanly
     is git's own definition of already-applied (what `git am`/`rebase` use), and
     it costs nothing on the success path — it only runs after a forward failure.
     A partial hand-merge fails both directions and stays CONFLICT. Correct.

4. **Fold (PICKABLE only)** — one chained selection on `cur`:
   `withChanges(changes: delta)` then
   `__withReplayedCommit(message, paths, date, authorName, authorEmail, origin: c.root)`.

   Commit paths come from the **delta**, not from `c.touched`: if a hunk landed
   on content that already matched, that path drops out and must not be named in
   the commit scope. Take `paths.Added + paths.Modified + paths.AllRemoved`, trim
   trailing `/`, drop `""`/`.`, prefix with `/` (withCommit resolves relative
   paths from `ws.Cwd` — `resolveWorkspacePath`, core/schema/workspace.go:3154),
   dedupe, sort. Renames cannot be split (`asPatch` uses `--no-renames`,
   core/changeset.go:849-853), so `workspaceCommitScope`'s split-rename refusal
   (:490-496) can never trigger.

### Why the whole-file overlay is safe here

The delta's `Before` is the receiver's **current** tree and its `After` is that
tree with the patch applied. Overwriting whole files with "current content +
patch" is exactly right; nothing older than the receiver's own state is ever
written. **Patch application is the merge; `withChanges` is just the write.**

### Helpers in the same file

```go
// overlappingPaths returns the members of dirty colliding with a touched path,
// in EITHER direction — a dirty file under a touched directory, or a touched
// file under a dirty directory. commitPathInScope (workspace_commit.go:652)
// only tests one direction, which is not enough here.
func overlappingPaths(touched, dirty []string) []string

// parsePatchConflictPaths pulls file names out of a `git apply` failure (see
// applyGitPatch, core/directory.go:1893, and gitDiagnostics.wrap, :1837).
// Falls back to the commit's touched paths, so conflictPaths is never empty.
func parsePatchConflictPaths(err error, touched []string) []string

func shortSHA(sha string) string       // 7 chars, for error text
func commitSubject(msg string) string  // first line, for error text
```

`parsePatchConflictPaths` patterns (case-sensitive), then dedupe/sort/intersect
with `touched` defensively:

| pattern | git message |
|---|---|
| `error: patch failed: (.+):\d+` | hunk did not apply |
| `error: (.+): patch does not apply` | file-level failure |
| `error: (.+): No such file or directory` | missing pre-image |
| `error: (.+): already exists in working directory` | added file already there |

## 5. The two resolvers

```go
type workspaceCommitsFromArgs struct {
	Source  dagql.ID[*core.Workspace]
	Commits []string `default:"[]"`
}

func (s *workspaceSchema) commitsFrom(...) (dagql.Array[*core.WorkspaceCommitPick], error)
	// load source, fold, project candidates to picks

func (s *workspaceSchema) withCommitsFrom(...) (dagql.ObjectResult[*core.Workspace], error)
	// load source, fold, then collect EVERY conflict into errors.Join:
	//   DIRTY: "commit %s (%q): the workspace has uncommitted changes on %s;
	//           commit, save or discard them first"
	//   else:  "commit %s (%q): no longer applies to %s"
	// wrapped as "withCommitsFrom: %d commit(s) cannot be applied: %w"
	// PICKED/REDUNDANT are silently skipped — by definition nothing.
```

Every conflict is reported, not just the first: an agent should fix the whole
batch in one round trip.

Return `dagql.Array[*core.WorkspaceCommitPick]` (raw objects) — precedent
`dagql.Array[*core.SearchResult]` (core/schema/workspace.go:1222).
`ObjectResultArray` + a `__…Entry` field (the `stagedCommits` shape) is only
needed when each element needs a real persistable ID; picks do not.

## 6. Schema surface — `core/schema/workspace.go`

Install into `dagql.Fields[*core.Workspace]` right after `__stagedCommit`
(ends :182): `commitsFrom`, `withCommitsFrom`, `__withReplayedCommit`.

Doc strings (write them in this spirit — they are LLM-facing):

- `commitsFrom`: "Plan which of another workspace's staged commits can be
  applied to this one." + "Both workspaces are expected to descend from the same
  checkout — typically this workspace and one an agent was spawned with. Each of
  the source's staged commits is classified against this one, oldest first, as
  if every pickable commit before it had already been applied: PICKED,
  REDUNDANT, CONFLICT (see reason and conflictPaths), or PICKABLE." +
  "Read-only: nothing is staged and neither workspace is modified. Pass the
  pickable hashes to withCommitsFrom to apply them."
- `withCommitsFrom`: "Return this workspace with another workspace's staged
  commits replayed on top, without mutating either source." + "Each commit is
  applied to this workspace's current content as a patch — not as a whole-file
  overlay — so commits still land cleanly when this workspace has moved on since
  the source branched off. The replayed commit keeps the original message, date
  and author identity, and records the original commit as its origin, so pulling
  the same work again is recognised as already present." + "Commits this
  workspace already has, and commits whose content is already present, are
  skipped. A commit that cannot be applied is an error naming the commit and the
  conflicting paths: plan with commitsFrom first and pass the pickable hashes."
- Args: `source` ("The workspace whose staged commits to consider/replay."),
  `commits` ("Restrict … to these commit hashes, full or an unambiguous prefix.
  They are always considered/applied in the source's stack order. Empty …").

Also: `srv.InstallObject(dagql.NewClass[*core.WorkspaceCommitPick](srv).View(AfterVersion("v1.0.0-0")))`
in the block at :414-420; `dagql.Fields[*core.WorkspaceCommitPick]{}.Install(srv)`
next to the `WorkspaceStagedCommit` one (:452); and both enum `Install(srv, AfterVersion("v1.0.0-0"))`.

Decisions:

- `View(AfterVersion("v1.0.0-0"))` on both public fields, the object and both
  enums — the whole workspace API is gated there (:53-412, :414-420) and the
  release line is v0.21.x. Enum precedent: `PatchConflicts.Install(srv, AfterVersion("v1.0.0-0"))`
  (core/schema/directory.go:39).
- `__withReplayedCommit`: **no** `View`, matching `__stagedCommit` (:173) and
  `__withGeneratedLocalDependencies` (:183).
- **No `DoNotCache`** on either: both are pure functions of the two workspace IDs
  and the hash list; host-sensitive reads underneath carry their own cache inputs
  (`git` is `PerClientInput` at :126). `withCommit` is likewise not DoNotCache.
  Adding it would defeat the plan→apply cache reuse that makes the shared fold
  affordable.
- **No `IsPersistable`**: it marks a field's *result* eligible for persistent
  cache storage (dagql/objects.go:911-912). What must survive is the Workspace
  payload's references, which come from `__stagedCommit` — already persistable.
- **No extra `WithInput`**, following `withCommit`.

## 7. Codegen + checks

- `generate` with `["go-client:generate", "docs:references"]` → `sdk/go/dagger.gen.go`
  (`WorkspaceStagedCommit` already at :18091) and `docs/docs-graphql/schema.graphqls`.
  No other checked-in generated artifact references `WorkspaceStagedCommit`
  except `docs/static/reference/php/*`, regenerated at release time — leave it.
- Checks: `golang:lint-all`, `test-split:test-workspaces`.
- Changelog: `.changes/unreleased/Added-<UTC timestamp>.yaml`, shaped like the
  existing entries (kind `Added`, body, `custom.Author`, empty `custom.PR`).

## 8. Tests — `core/integration/workspace_commit_from_test.go` (NEW)

Methods on the existing **`WorkspaceSuite`** (declared in
`core/integration/workspace_test.go`) so `test-split:test-workspaces` picks them
up. Existing commit coverage: `core/integration/workspace_commit_test.go`
(`withCommitBase` :24, `commitTestDate` :21 — reuse the date constant).

**Fixture — two workspaces in one session.** The existing tests drive
`currentWorkspace` through `daggerQuery` in a container, one session per exec.
That cannot express "pass workspace B as an argument to workspace A" (GraphQL
can't feed one field's result into another's argument, and `currentWorkspace` is
`NotReplayable`, core/schema/workspace.go:37). Use **Directory-backed (value)
workspaces** via the Go SDK: a real git repo carried as an in-engine Directory,
`chief := base.AsWorkspace()`, worker `:= chief.WithNewFile(…).WithCommit(…)`.
That exercises the whole path (`ensureWorkspaceGitDirectory`,
`workspaceCommitBaseRepo`, `overlayEdit`'s value branch :2550, `git.uncommitted`
via the overlay :2924-2931) with both workspaces as ordinary values.

Cases:

1. `…FreshPull` — one pick, PICKABLE, metadata verbatim, `changes.diffStats`
   naming the file. After apply: staged sha **differs** from source (rewritten
   parent), `origin` **equals** source sha, metadata verbatim, `git.head.commit`
   matches, file content matches, `git.uncommitted.isEmpty`.
2. `…ReplayIsIdempotent` — re-plan → all PICKED; re-apply → still one commit.
3. `…InheritedCommitIsPicked` — chief stages C, worker derives from it and adds
   W: plan `[C: PICKED, W: PICKABLE]`; after apply `[C (original sha), W' (origin=W)]`.
   (The common case: a worker's stack starts as a copy of the chief's.)
4. `…DriftStillApplies` — chief commits an edit to line 10, worker's commit edits
   line 1 → PICKABLE, both edits present after apply. **This is the test that
   fails under naive whole-file application.**
5. `…ContentConflict` — chief commits a different edit to the *same* line →
   CONFLICT/CONTENT, `conflictPaths == ["a.txt"]`; apply errors naming the file,
   short sha and subject.
6. `…DirtyPathRefusal` — chief has an *uncommitted* edit to a path the worker's
   commit touches → CONFLICT/DIRTY; apply errors mentioning "uncommitted
   changes"; assert the chief's dirty content is intact and nothing was swept in.
7. `…DependentCommitCascades` — worker stages W1 (creates new.txt) then W2 (edits
   it); chief independently committed a different new.txt → both CONFLICT; apply
   errors listing both; chief's file untouched.
8. `…RedundantHandMerge` — chief made the identical edit under a different
   message → REDUNDANT (the case the reverse probe exists for); apply is a silent
   no-op, staged count unchanged.
9. `…OriginIsTransitive` — A→B→C, then a direct pull from A → PICKED, stack
   length unchanged; assert the third generation's origin is the **root**.
10. `…PreservesCommitMetadata` — multi-line message, distinct date, author
    name/email differing from the receiver's identity: all four verbatim, and the
    receiver's git identity did not override them.
11. `…SelectsRequestedCommits` — cherry-pick by full sha and by 7-char prefix;
    the requested order never overrides stack order.
12. `…RejectsUnknownCommit` — an unknown hash errors naming it (not a no-op).
13. `…EmptySource` — no staged commits: plan `[]`, apply unchanged, no error.
14. `…PlanIsReadOnly` — after planning, the receiver's stack and contents are
    unchanged.
15. `TestWorkspaceCommitsFromHostCheckout` — the one **host-backed** end-to-end
    case, container style, via `daggerShell` (core/integration/shell_test.go:34)
    since it runs a single session and can bind an object to a shell variable:
    stage a commit in a derived workspace, `with-commits-from $worker | export`,
    then assert on the host with `gitOut(…, "log", "-1", "--format=%an <%ae>%n%ad%n%s")`
    (helper at workspace_commit_test.go:580) that the checkout got the replayed
    commit with the **original** author and date, and that an unrelated pending
    edit is still uncommitted. Only this case exercises sparse host
    `resolveRootfs`, `exportPendingCommits` and the `BaseHeadSHA` invariant
    (workspace_commit.go:396-401).

## 9. Risks / sharp edges

1. **Commits staged in a workspace with no overlay cannot be replayed.**
   `withCommit` only records `pending.Committed` when the workspace has an
   overlay (core/schema/workspace_commit.go:163), so `stagedCommitChanges`
   (:305-320) reports an *empty* changeset for such a commit — a pre-existing
   limitation of `stagedCommits[i].changes`. Treating that as REDUNDANT would
   drop real work, so the fold **raises a clear error** instead. Cannot arise for
   a worker that edited anything before committing. Follow-up fix: record the
   repository-anchored scoped changeset even with no overlay, or derive the patch
   with `git diff --binary <sha>^ <sha>` in `commit.Repo` — the latter must go
   through the same normalisation as `writeGitDiffPatch`/`fixDiffGitHeader`
   (core/changeset.go:848, :942) or `withPatch` will reject it.
2. **A planned CONFLICT leaves failed spans in the trace** — detection *is* a
   failed `git apply` (twice, counting the reverse probe). `enginetel.Task`
   wrapping puts them under an internal "apply patch" span. If too noisy, the
   follow-up is a non-failing probe (`Directory.patchConflicts(patch:)` backed by
   `git apply --check`), which would also give exact paths without string parsing.
3. **`conflictPaths` for CONTENT comes from parsing git's stderr** — formats are
   stable but not contractual; mitigated by the fallback to touched paths, so the
   field is never empty and only ever less precise.
4. **`merge-base --is-ancestor` reads any git failure as "absent"** — degrades
   PICKED detection into content classification, the safe direction.
5. **After the chief SAVES a pulled commit and reloads, the origin link is gone**
   (it is engine-side metadata; the saved sha differs from the source's). Re-pull
   then relies on REDUNDANT to notice. Works when the content is untouched; if
   the chief has since modified those files, a duplicate commit is possible. A
   durable fix needs the origin in the commit object (trailer or git note) —
   out of scope, recorded deliberately.
6. **Empty added directories are lost** — `asPatch` cannot express them
   (core/changeset.go:672-681); such a commit classifies REDUNDANT and is skipped.
7. **Cache mounts are a hard error, not a CONFLICT** — `applyChangeset` refuses
   changeset paths under a cache mount (core/schema/workspace.go:1917-1919), and
   that happens inside the fold, so both fields raise. Consistent (plan and apply
   agree) but arguably deserves its own status later.
8. **The dirty set is computed once** — see the invariance argument above.
9. **The planner does the full apply work.** The price of one shared
   implementation, and bounded: every step is a real dagql field call, so the
   following `withCommitsFrom` hits cache. But `commitsFrom` is not a cheap read;
   the module should not call it every turn.
10. **Cross-client evaluation** — the fold reads the *source* only through
    already-materialized in-engine values (its recorded `Committed` changesets),
    never through its host, so a worker's snapshot workspace is classified without
    routing to its client. Only receiver reads go through
    `withWorkspaceHostReadContext`. Preserve this: pulling the source's host in
    would make the operation depend on a second live client.

## 10. Decisions a reviewer may want to revisit

- An internal `__withReplayedCommit` rather than an `origin` arg on public
  `withCommit` (a public arg is meaningless to normal callers) or an in-Go
  construction (unsafe: N workspaces would share one call ID).
  `__withGeneratedLocalDependencies` is the precedent for "internal variant of a
  public edit with extra bookkeeping".
- The reverse-apply probe is what makes REDUNDANT real (a deviation from the
  original "empty changeset" plan, which is kept as the fast path).
- `git.unmanaged` folded into the dirty set alongside `git.uncommitted`.
- `reason` as a non-null enum with `NONE`.
- `WorkspaceCommitPick.changes` included.
- Short-hash prefixes (>=7) accepted; unknown/ambiguous hashes are errors,
  because a typo must not become a silent no-op.
- `commits` never reorders; stack order always wins.
- `withCommitsFrom` with empty `commits` is **still strict**: "pull everything"
  is not a safe blind call, by design.

---

# Part 2 — `modules/staff` harvest tools

Five new chief-side tools on `type Staff`, each taking the chief's own
`source: Workspace!` (auto-filled, hidden from the model) plus the worker `name`.
None touches `Staff` state, so none returns `Staff!`.

The worker's workspace is reached via `member(name).snapshot.workspace`
(`Agent.snapshot` is the loop's last committed conversation; `LLM.workspace` its
bound workspace) — readable while the worker runs and on a stopped worker's
tombstone.

## Tools

| tool | returns | what it does |
|---|---|---|
| `logOf(name, limit = 10)` | `String!` | the worker's staged commits, oldest first, each annotated with what `pull` would do: `new` / `already had` / `redundant` / `CONFLICT` + reason + paths. Rendering modeled on `modules/history`'s `log`. Header line carries the whole-plan counts even when `limit` truncates. |
| `diffOf(name, paths = [], commit = null)` | `String!` | with `commit`: that staged commit's metadata + message + patch. Without: the worker's UNCOMMITTED work as diffStats summary + patch (+ an `unmanaged` block). `paths` scopes via the patch-section slicer. Modeled on `modules/review`. |
| `pull(name, commits = [])` | `Workspace!` | plan via `commitsFrom`, apply ONLY the PICKABLE set via `withCommitsFrom`, `print()` a report. Empty `commits` = everything new; explicit = cherry-pick. |
| `pullConflicted(name, commit)` | `Changeset!` | recovery for ONE conflicting commit: apply its patch to the chief's tree with `LEAVE_CONFLICT_MARKERS`, landing UNCOMMITTED edits, and print the original message to reuse. |
| `pullPending(name, markers = false)` | `Changeset!` | the worker's uncommitted (+ unmanaged) work, re-anchored on the chief's tree. `FAIL` by default, `markers: true` to leave conflict markers. Also the "rescue a dismissed worker's WIP" tool. |

**All five carry `@cache(policy: FunctionCachePolicy.Never)`** — not because they
mutate, but because they read the worker's **live snapshot**, which is not part
of the call's arguments. `logOf(name: "scout")` twice must not replay the first
plan. Write that reason into each body as a comment, matching the existing idiom.

### Semantics worth getting right

- **`pull` never applies a CONFLICT.** It reports each one with reason, paths and
  the recovery route:
  - DIRTY → "commit or revert your edits on those paths and pull again (keeps the
    worker's authorship); or `pullConflicted` to take it as your own edits"
  - CONTENT → "`pullConflicted` lands it as uncommitted edits with conflict
    markers; resolve them and commit it yourself"
- **`pullConflicted` is deliberately NOT a mode of `pull`.** A commit must never
  be staged with conflict markers inside it — that would put a broken tree in
  history under the worker's name. So the resolution lands in the working tree
  and the commit that records it is the chief's; the original message is printed
  for reuse. If the target actually applies cleanly, print a NOTE steering back
  to `pull` (advisory, not a refusal) since that keeps the worker's authorship.
- **`pullPending` must re-anchor by intersecting with tree drift.** Naively
  applying the worker's `uncommitted` patch to the chief's tree fails in the
  COMMON case: the worker inherited the chief's pending edits at spawn, so its
  patch contains hunks the chief already has, and `withPatch` is a plain
  `git apply` (core/directory.go:1893 — no `--3way`), which rejects
  already-applied hunks. So compute
  `theirs.directory("/").changes(from: source.directory("/")).diffStats` — the
  paths where the two trees genuinely differ — and scope the patch to those files
  with the same section slicer used for `paths`. Partial overlap *inside* a file
  still fails; that is what `markers: true` is for, and the failure message says
  so.
- **Short shas are first-class.** `logOf` prints 7-char shas, so `diffOf`,
  `pull` and `pullConflicted` resolve full-or-short shas module-side against the
  worker's staged-commit list (`resolveCommit(known, given, name)`: exact match,
  else unique-prefix filter; empty → "run logOf to list them", multiple → "use a
  longer sha") before calling the core API.
- **`logOf` shows no per-file stats** — `WorkspaceCommitPick.changes` exists, but
  forcing diffStats for the worker's whole stack to render a log is not worth it.
  The plan status *is* the payload; detail is one `diffOf(name, commit:)` away.
  Say so in the docstring.

## Insertion points in `modules/staff/main.dang`

| what | where |
|---|---|
| the five tools, in the table's order | **after `collect` (ends :210), before `interruptWorker`** — the file then reads steer → observe → collect the reply → harvest the work → interrupt/dismiss |
| `let tombstones` field | immediately after `let members` (:26) |
| private helpers + string constants | **after `memberNames` (ends :269), before `chiefPrompt`** |
| chiefPrompt paragraph | inside `chiefPrompt`, after the existing bullet list, before "Workers can ask YOU questions" |

Helpers (all `let`, i.e. private → never bound as tools, so **`plumbing` needs no
change** — say so in the commit message so it doesn't read as an oversight):

- `workerWorkspace(name) -> Workspace!` = `harvestTarget(name).snapshot.workspace`.
  **No rescue here** — it only BUILDS a query; a rescue on a lazy chain can never
  fire and the compiler says so.
- `resolveCommit(known: [String!]!, given: String!, name: String!) -> String!`
- `scopePatch(patch: String!, paths: [String!]!) -> String!` — the "diff --git "
  section slicer, copied from `modules/review`'s `diff` (including the
  `a/`/`b/` prefix tolerance).
- `pickLabel(status, reason)`, `conflictReason(reason)`,
  `conflictRecovery(reason, name, sha)` — `case` over the new enums with a
  trailing `else` arm; declare the enum params **nullable** (like history's
  `renderKind(kind: DiffStatKind)`) so the module compiles whichever nullability
  the core lands on.
- `short(sha)`, `subject(message)`, `pathNote(paths)`, `renderKind(kind)`,
  `patchHeader`, `unmanagedHeader` — copied from history/review.

## Changes to existing code in the module

- **File header comment (:1-19)**: extend the steering list to mention the
  harvest family and that it reaches `member(name).snapshot.workspace`.
- **`dismiss` docstring**: add a HARVEST FIRST warning — a worker's commits and
  edits live in ITS workspace; `pull` what it committed and `pullPending` what it
  did not, before letting it go.
- **Tombstones (recommended, ~10 lines, separate commit).** Without it, "rescue a
  dismissed worker's WIP" is impossible: `dismiss` drops the handle from
  `members`, so `member(name)` raises and the Agent tombstone (readable for the
  rest of the session) is unreachable. Add `let tombstones: Map[Agent!]! = [:]`;
  `spawn` clears any entry for a re-hired name; `dismiss` moves the handle into
  it before removing from `members`; harvest tools resolve through a new
  `harvestTarget(name)` (live members first, then tombstones, else a raise that
  mentions both). **Steering tools keep using `member()`** so they can never
  resolve to a corpse.
- **`HANDOFF.md` open thread #2** ("Worker workspace isolation … possible future
  staff tool") is what this closes — strike it or mark it done.

## chiefPrompt addition (after the existing bullet list)

Teach: workers edit and commit in their OWN copy of the workspace and nothing
reaches the chief's until harvested; one bullet per tool (as in the table);
then two rules —

- **COLLECT BEFORE THE FINAL HARVEST**: these tools read the worker's last
  COMMITTED conversation step, so a mid-turn worker may hold work they cannot
  see yet. `collect` (or `interruptWorker`) before the harvest that matters, and
  harvest BEFORE `dismiss`.
- **The conflict loop**: `pull` → read the CONFLICT block → per entry, either
  commit/revert your own edits on the named paths and pull again (DIRTY — keeps
  the worker's authorship), or `pullConflicted`, resolve markers, commit it
  yourself (CONTENT). `logOf` after every round shows what is left.

## Dang correctness notes (verified live against a from-source engine)

- **`dagger -m <mod> functions` does NOT type-check Dang bodies** — the Dang
  SDK's `ModuleTypes` path resolves imports and signatures only
  (`core/sdk/dang/v2/sdk.go:60`); a hard type error in a body still exits 0.
  **The real gate is `dagger -m ./modules/staff call status`** — whole-module
  inference, and the only path that prints warnings. Expect `(none)` and **zero
  warnings**; treat a laziness warning as a bug in rescue placement.
- `agent.snapshot.workspace` type-checks as `Workspace!`. (`LLM.workspace`
  *raises* for an unbound LLM — impossible for staff workers.)
- A leaf reached through a **nested** `{{ }}` keeps non-null typing:
  `c.changes.asPatch.contents` from
  `stagedCommits.{{ sha, changes.{{ asPatch.{{ contents }} }} }}` feeds a
  `String!` arg directly. `?? ""` is also accepted without a never-null warning,
  so it is a safe defensive spelling.
- `commit: String = null` + `if (commit != null) { … }` narrows to `String!`
  inside the branch (verified at runtime too).
- **List indexing yields a nullable**: `xs[0]` in a `String!` slot is a type
  error and `xs[0] :: String!` is rejected *statically*. Use `xs[0] ?? ""` or
  `xs.takeFirst.join("")`. Hence the `hits.map { … }.join("")` fold wherever a
  single record is needed.
- `s.kind == DiffStatKind.ADDED` on a record from
  `git.uncommitted.diffStats.{{…}}` type-checks directly; `.filter/.map/.reduce`
  over such records is fine.
- **Forcing + rescue**: `changes.isEmpty rescue { e: Error => raise … }` is
  properly connected (`isEmpty` is a scalar leaf = an execution point) and emits
  no laziness warning. The warning channel is real, so silence is meaningful.
- `theirs.directory("/").changes(from: source.directory("/")).diffStats.{{path}}.map { s => s.path }`
  works — the drift computation.
- `base.changes(from: base)` is a valid empty changeset — the "nothing to take"
  return.
- `base.withPatch(p, onConflict: PatchConflict.LEAVE_CONFLICT_MARKERS).changes(from: base)`
  works; `PatchConflict.FAIL` likewise.

**Where things force, and what rescue must not swallow:**

- `logOf`/`diffOf` force at their `{{ }}` selections — no rescue; a failure there
  is a genuine error worth surfacing raw.
- `pull` forces `withCommitsFrom` via `staged.git.head.commit`.
- `pullConflicted`/`pullPending` force `withPatch` via `changes.isEmpty`.
- Without those forces the errors land *outside* the module — when the engine
  rebinds the Workspace or overlays the Changeset — stripped of context.
- **Every rescue re-raises with an enriched message embedding `e.message`. None
  may return a fallback value.** A swallowed `withCommitsFrom` failure would
  report a pull that did not happen; a swallowed `withPatch` failure would
  silently discard the worker's work. No `rescue null` anywhere.

**Other Dang notes:**

- Iterate on materialized records from ONE batched `{{ }}` per source (list
  methods work on records, not on raw GraphQL object lists — history says so).
  Counts via `.filter{}.length`; single records via `.filter{}.map{}.join("")`.
- **Never pass an anonymous record (or list of them) to a helper** — ad-hoc
  record types have no spellable name. Helpers take scalars, enums, `[String!]!`.
- Backtick templates don't interpret `\n` — use `+ "\n" +`.
- `print()` output is prepended to the automatic state-return summary, joined by
  `\n---\n` (core/llm_object_tools.go:703, core/mcp.go:453), for both `Workspace!`
  and `Changeset!` returns. Nothing is lost by returning state instead of String.
- stdlib used (all in the skill's reference): `split`, `trimPrefix`, `hasPrefix`,
  `contains`, `join`, `takeFirst`, `takeLast`, `map`, `filter`, `any`, `reduce`,
  `uniq`, `length`, `isEmpty`, `Path()`/`Path.contains`, `toString`, `print`,
  `raise`; map `[k]`, `.with`, `.without`, `.has`, `.isEmpty`, `.keys`.

## Module-side risks

1. **The snapshot is the last COMMITTED step** — a RUNNING worker's in-flight
   step is invisible until the next step boundary. Every docstring and the chief
   prompt say "collect or interruptWorker first for a final harvest". Possible
   follow-up: print the worker's `state` when RUNNING as a nudge.
2. **STOPPED workers across sessions** — the runtime registry is per-session; a
   tombstone re-selected in a NEW session projects IDLE-from-absence and its
   `snapshot` is the SEED conversation, i.e. the spawn-time workspace. Harvest
   would silently return "nothing new" rather than erroring. Harvest within the
   session.
3. **`pullPending`'s double-apply hazard** (the biggest): `withPatch` is plain
   `git apply`, no 3-way. The drift intersection removes whole-file overlap (the
   common case); it cannot remove *partial* overlap inside a file both edited.
   Shipped mitigation: honest FAIL pointing at `markers: true`. Real fix if it
   bites: a 3-way `onConflict` mode, or a `Workspace.pendingFrom(source:)`
   planner sibling to `commitsFrom`.
4. **Renames** — patch scoping matches on `DiffStat.path` only, so a renamed
   file's old path can be dropped from a scoped patch, leaving a half-applied
   rename. `modules/review` has the same limitation, but `pullPending` *applies*
   rather than displays, so the consequence is bigger. Cheap hardening: select
   `oldPath` too and add it to the keep-set.
5. **Worker with no staged commits** (very common — workers often never commit):
   `commitsFrom` returns `[]`, `logOf` says so and points at `diffOf`/
   `pullPending`, `pull` prints "has no staged commits to pull" and returns
   `source` unchanged.
6. **Explicit `commits:` with unmet dependencies** cherry-picked out of order
   classifies CONFLICT/CONTENT; the message is accurate but not obviously about
   ordering. Possible follow-up: detect "you skipped an earlier PICKABLE commit".
7. **Workspace-returning `pull` races** — rebinding replaces the chief's whole
   workspace; a concurrent tool in the same batch that also returns a Workspace
   could lose edits. Inherent to the convention (`editor.commit` has it too).
8. **Tombstone map growth** — one handle per dismissed name, forever, in
   serialized state. Bounded by distinct dismissed names per session; cap or add
   `forget(name)` if it ever matters.
9. **`pullConflicted` attribution** — the resolution is committed by the chief,
   so the worker's authorship is lost by construction. The printed message is the
   mitigation.

## Verification

1. `dagger -m ./modules/staff call status` — the real type-check gate. `(none)`,
   zero warnings.
2. Live loop: hire a worker, have it write a file and commit, then
   `logOf`/`diffOf`/`pull`/`pullPending` from the chief. Multi-step stateful
   agent scenarios need ONE session (the runtime registry is per-session) —
   `dagger shell -c` scripts.
3. Manual cases: worker with zero staged commits; pull twice (second reports
   "already had"); chief edits a path the worker committed (DIRTY →
   `pullConflicted`); worker deletes a file; `pullPending` failing with
   `markers: false` and succeeding with `markers: true`.
