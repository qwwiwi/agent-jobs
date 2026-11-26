# #240: safe reclamation of finished jobs' temp dirs – design after two BLOCKs

Status: proposal (design only, no product code). Base: `751993e` (v2, "Prune a finished job's temp dir
after 2 hours"). Author: research job 261126-055426-848a.

## 1. Problem and history

Finished jobs keep `<job dir>/tmp` (their `TMPDIR`: pytest PostgreSQL clusters, venvs, go build dirs) until
the 48 h prune. On the box this reached about 74 GB and filled the disk.

| Attempt | Idea | Blocked on |
|---|---|---|
| v1 `725b233` | a new remover that runs when the job settles | who retired the dir, ancestor and mount boundaries, rename/relocation races, race with resume, writers that survive |
| v2 `751993e` | the existing prune path with a 2 h retention, pass every 10 min | (a) `removeTree` crosses mount points, (b) a pending scope stop is not checked, (c) doctor's prune dry run walks temp trees unbounded under the lifecycle lock; low: `partial` missing in the aggregate |

v2's shape is right: it reuses the per-job lifecycle lock, the row re-read and the eligibility checks, so
v1's "who retired the dir" and "race with resume" questions do not arise. What it lacks is three properties
of the code it reuses. Two of them, (a) and (b), are defects of today's 48 h prune as well; v2 only makes
them 24 times more likely to be hit.

## 2. Chosen design in one paragraph

Keep v2. Fix the three shared weaknesses once, at the place every caller goes through:

1. **One bounded remover.** `removeTree` stops using `os.RemoveAll` and becomes a descriptor-relative walk
   in which every directory is opened with `openat2(parent_fd, name, RESOLVE_NO_XDEV | RESOLVE_BENEATH |
   RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS)`. A mount point is never entered and never removed; the
   call fails closed. All 12 call sites (48 h prune, 2 h temp pass, sessions, advisor dir, archive, sources,
   manifest) get it without a change.
2. **One hold rule.** A single function answers "may the daemon touch this finished job's files?" –
   pending scope stop, pending advisor calls, live spawned group, live acceptance group. Cleanup, resume and
   prune all call it, so prune can no longer miss a condition the other two wait for.
3. **Doctor reads, it does not walk.** `Disk()` stops calling `Prune(true)`. Prunable bytes come from the
   sizes already recorded on the rows, without the lifecycle lock. The CLI dry run keeps its exact
   eligibility checks but measures with the existing bounded walk instead of `treeSize`.

No new config key, no new table or column, no new dependency (`golang.org/x/sys` is already required and
has `Openat2`).

## 3. What is verified and what is inferred

Verified (opened in this job):

- `internal/jobs/prune.go:596-602` – `removeTree` is `os.RemoveAll`, then `chmodTree`, then `os.RemoveAll`
  again. No device or mount check anywhere.
- `go doc os.RemoveAll` (go1.27.2) – documents no filesystem boundary. `go doc os.Root`: "Methods on Root
  do not prohibit traversal of filesystem boundaries, Linux bind mounts, /proc special files…" – so
  `os.Root.RemoveAll` is not a fix either.
- `man 2 openat2` on the host (kernel 6.8.0-138): `RESOLVE_NO_XDEV` – "Disallow traversal of mount points
  during path resolution (including all bind mounts)"; error `EXDEV` – "a path component crosses a mount
  point"; available since Linux 5.6.
- `go doc golang.org/x/sys/unix.Openat2`, `OpenHow`, `RESOLVE_*` exist in the pinned v0.48.0.
- `internal/jobs/prune.go:224-252` – `pruneJob` checks `advisorPending`, `spawnedGroupAlive`,
  `acceptanceGroupAlive`, `CleanedAt`; it never reads `j.ScopePending`.
- `internal/jobs/finish.go:720` – `cleanupDue` skips a job while `j.ScopePending || m.advisorPending(j)`;
  `finish.go:866-869` (`Cleanup`) checks the two groups; `resume.go:848` and `resume.go:1041` refuse on
  `ScopePending`. `store.go:133-135`: the flag means "the row is terminal, but processes of the job may
  still run in its scope".
- `internal/jobs/system.go:121` – `Disk()` calls `m.Prune(true)`; that reaches `pruneOne` (lock at
  `prune.go:183`), then `treeSize` at `prune.go:307` and `prune.go:333`. `treeSize` (`prune.go:467-479`) is
  an unbounded `filepath.WalkDir`. Since v2 the dry run also includes every temp dir past 2 h.
- `internal/jobs/system.go:80-87` – `JobDirsPartial` is set only from the live walk of running jobs; a
  recorded `j.DirPartial` reaches the per-job entry but not the aggregate (the Low finding).
- `man systemd.scope` (systemd 255): scope units "manage externally created processes, and do not fork off
  processes on its own" – a scope has no `ExecStopPost=`.
- `internal/jobs/scope.go:34` – the fallback without `systemd-run --user` (macOS, containers) is
  "session only" containment: no cgroup proof that the job's processes are gone.
- The job guard refuses `unshare` (tried once for a mount experiment, refused, not retried).

Inferred (not run, reasoned from the above):

- `os.RemoveAll` descends into a mounted filesystem and deletes its content. Basis: its documentation
  names no boundary and the code path has no device check; the Go team documents the same gap for
  `os.Root`. Not demonstrated empirically here – the experiment needs a mount namespace, which the guard
  forbids inside a job.
- A same-filesystem bind mount has the same `st_dev` as its surroundings, so a per-directory `st_dev`
  comparison alone misses it. Basis: `st_dev` identifies the filesystem, not the mount; `man 2 openat2`
  singles out bind mounts as covered by `RESOLVE_NO_XDEV`. This is the reason for choosing `openat2` over
  the `st_dev` check on Linux.
- `rmdir`/`unlink` of a mount point fails with `EBUSY`, so a refused directory also survives its parent's
  removal (the parent then fails with `ENOTEMPTY`, which is the wanted outcome).
- macOS has no same-filesystem bind mounts, so `st_dev` is a sufficient boundary there.

## 4. Design details

### 4.1 Bounded remover (closes High a)

New file `internal/jobs/removetree.go`, with the opener split by OS
(`opendir_linux.go`, `opendir_other.go`). `removeTree(p string) error` keeps its signature.

```
removeTree(p):
  parent := open(dir(p), O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)   // daemon-owned path
  root_dev := fstat(parent).st_dev
  return removeAt(parent, base(p), depth 0)

removeAt(dfd, name, depth):
  st := fstatat(dfd, name, AT_SYMLINK_NOFOLLOW)          // ENOENT -> nil
  if st is not a directory: return unlinkat(dfd, name, 0) // symlink removed as a link; EBUSY reported
  fd, err := openDirBeneath(dfd, name)                   // the only way a directory is ever entered
  if err is EXDEV:  return ErrMountBoundary{path}        // not entered, not removed
  if err != nil:    return err                           // EACCES, ELOOP, ENOSYS...: fail closed
  if fstat(fd).st_dev != root_dev: return ErrMountBoundary{path}   // second, portable check
  if fstat(fd).mode lacks owner rwx: fchmod(fd, 0700)    // through the descriptor, replaces chmodTree
  for each entry of fd: err = join(err, removeAt(fd, entry, depth+1))   // siblings continue
  if err != nil: return err                              // something below was kept: keep this dir too
  return unlinkat(dfd, name, AT_REMOVEDIR)
```

`openDirBeneath` on Linux is `unix.Openat2(dfd, name, &OpenHow{Flags: O_RDONLY|O_DIRECTORY|O_NOFOLLOW|
O_CLOEXEC, Resolve: RESOLVE_NO_XDEV|RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS})`. On other
systems it is `openat(O_NOFOLLOW|O_DIRECTORY)`; the `st_dev` comparison is then the boundary.

Rules:

- **Fail closed.** `ENOSYS`/`EPERM` from `openat2` (kernel older than 5.6, a seccomp profile) is an error,
  not a reason to fall back to an unchecked open. Nothing is removed, prune reports it, the doctor shows
  the job as overdue (4.3).
- **A mount is skipped, its siblings are not.** The mounted directory and its ancestors up to the root of
  the removal stay; everything else in the tree is removed. The call returns `ErrMountBoundary` naming the
  path, so the job appears in the prune report with an error. Each open is checked on its own, so
  continuing with siblings never crosses anything.
- **No check-then-act gap.** The kernel applies `RESOLVE_NO_XDEV` during the same lookup that returns the
  descriptor. A mount that appears on a child after its parent was opened is refused when the child is
  opened. A mount placed on a directory that is already open does not change what the descriptor refers to
  (the underlying directory), so the walk stays on the original filesystem.
- **The same opener for the read-only walks.** `dirWalk.size` and the removal's chmod step use
  `openDirBeneath` too (`openDirNoFollow` for children becomes a thin wrapper), so a size measurement does
  not wander into a mounted filesystem either. `chmodTree` is deleted: its job is done inside the walk.
- **Freed bytes come from the walk.** `removeAt` sums `st_size` of what it unlinks and returns it; the
  callers in `prune.go` stop calling `treeSize` before a real removal. One walk instead of two under the
  lock.
- **Bounded hold.** The walk takes the same kind of budget `dirWalk` has (entries and a deadline; proposed
  60 s per job per pass). When it runs out, the walk stops, the job is reported as
  `Skipped: "removal continues on the next pass"`, and the next 10-minute pass continues. Removal is
  idempotent, so nothing needs to be remembered.
- Depth is bounded by the existing `maxChmodDepth` (256, one descriptor per level); deeper trees return an
  error and stay.

Why this closes (a): the only instruction that enters a directory is `openat2` with `RESOLVE_NO_XDEV`, and
`unlinkat` cannot remove a mount point. This holds for the 2 h temp pass at `prune.go:335` and for today's
48 h prune at `prune.go:309`, `:361`, `:394`, and for the callers in `archive.go`, `sources.go` and
`manifest.go`.

It also answers three of v1's findings for the removal itself: ancestor boundaries (`RESOLVE_BENEATH` and
`RESOLVE_NO_SYMLINKS`: no `..`, no symlink, at any step), mount boundaries, and a directory swapped for a
symlink mid-walk (already true of `os.RemoveAll`, now explicit).

### 4.2 One hold rule (closes High b)

New function in `manager.go`:

```go
// lifecycleHold says why the files of a job that is no longer running must not be touched yet
// ("" = free): processes of the job may still be alive.
func (m *Manager) lifecycleHold(j *store.Job) string
```

It returns, in this order: scope stop pending (`j.ScopePending`), advisor calls pending
(`m.advisorPending(j)`), spawned group alive, acceptance group alive. Callers:

- `pruneJob` (`prune.go:224-248`): the three separate checks become one call; `pj.Skipped` carries the
  reason. This adds the missing `ScopePending` case for both the temp pass and the 48 h prune.
- `cleanupDue` (`finish.go:720`) and `Cleanup` (`finish.go:866-869`): same call.
- `Resume` (`resume.go:848`) and `prepareResume` (`resume.go:1041`): same call; their user-facing error
  texts stay as they are (map the reason to the existing messages).

Why this closes (b): the rule `finish.go:720` applies is now the rule prune applies, by construction, and
a future fifth condition is added in one place. A job whose scope stop keeps failing keeps its temp dir;
that is correct (its processes may be writing there) and visible (4.3).

### 4.3 Doctor without the lock or the walk (closes High c and the Low)

`Disk()` (`system.go:106-134`) drops `m.Prune(true)`. `jobDirs` already lists every row; it also computes:

- `PrunableBytes` – an estimate from recorded sizes: for a finished job past the temp retention,
  `DirTmpBytes`; for one past 48 h whose records are not archived yet (`existingArchiveDir`, one `lstat`),
  `DirBytes`. No walk, no lifecycle lock. The JSON field keeps its name; its description says "estimate
  from recorded sizes".
- `PruneOverdue` (new, count) and `prune_error` text: jobs finished more than `retention + 3 ×
  TmpPruneInterval` ago that still record `DirTmpBytes > 0`. That is direct evidence that the pass is
  skipping or failing (pending scope, mount boundary, `openat2` unavailable, read-only dir), derived from
  rows alone, so it works from the MCP server process, which is not the daemon. `system_doctor` lists it
  under problems with the oldest job id; the reason is in the daemon log and in `agent-jobs prune
  --dry-run`.
- `JobDirsPartial = JobDirsPartial || u.Partial` for every row, not only running ones (the Low finding).

`agent-jobs prune --dry-run` and `PruneContext(ctx, true)` keep the exact per-job eligibility under the
lock (cheap: row read, a few `lstat`, group checks) but replace `treeSize` with the bounded `dirWalk`
(200 000 entries / 2 s, already used when a job settles). A capped measurement sets a new
`PruneEntry.Partial`, and `PruneReport.Partial` aggregates it. `treeSize` is deleted.

Why this closes (c): the doctor path no longer takes the lifecycle lock and no longer opens a single
directory of a finished job. The longest lock hold left on any dry-run path is one bounded walk (2 s) per
job, with the lock released between jobs as today.

## 5. Why not the alternatives

| Alternative | Verdict | Reason |
|---|---|---|
| Per-directory `st_dev` check only | rejected as the Linux mechanism, kept as the second check and as the non-Linux mechanism | misses same-filesystem bind mounts (inferred, section 3); needs an extra `fstat` to be race-free, while `openat2` does it in the lookup |
| `os.Root.RemoveAll` | rejected | documented not to stop at filesystem boundaries or bind mounts (verified) |
| systemd scope `ExecStopPost=` cleanup | not possible | scopes run no commands (verified, `man systemd.scope`). Moving jobs to transient *service* units would allow it, but that is a rewrite of the launch, of `scope.go` and of the session-only fallback, and the cleanup would run as an unsupervised `rm` outside the lifecycle lock – v1's resume race again |
| `TMPDIR` on a size-capped tmpfs | rejected for now | mounting needs root or a mount namespace per job; tmpfs is RAM and swap, and the offenders are multi-GB PostgreSQL clusters and venvs on a VDS. Caps usage but turns a full disk into `ENOSPC` inside jobs or into memory pressure |
| Per-job size cap on disk (project quota, loop file, btrfs subvolume) | later | all need root or a specific filesystem at install time; this is the "OS-level isolation" PLAN.md I9 already defers. It is the only real answer to "one job writes 60 GB in an hour" (section 7) |
| Removal when the job settles (v1) | stays rejected | every v1 finding returns; the 2 h pass under the lock gives the same disk outcome with a delay |
| Retention 0 / shorter than 2 h | not now | changes nothing about safety after this design, but a judge or a resumed attempt may still want the dir; revisit with data from `job_tmp_bytes` |

## 6. Slice plan

All slices are commits on the v2 branch before it merges. Test first in every slice (red run recorded,
then green). Each slice ends with a green `scripts/check.sh` on its head.

### S1 – bounded remover (High a). About 5 h

Files: new `internal/jobs/removetree.go`, `opendir_linux.go`, `opendir_other.go`; `prune.go` (`removeTree`
body, `chmodTree` removed, `dirWalk.size` opener, freed bytes from the walk, budget); `jobfile.go`
(`openDirNoFollow`).

Failing tests first (`internal/jobs/removetree_test.go`):

- `TestRemoveTreeStopsAtMountBoundary` – the opener is a package variable; the test makes it return `EXDEV`
  for one directory. Expect: that directory and its content intact, its ancestors kept, siblings removed,
  `errors.Is(err, ErrMountBoundary)`.
- `TestRemoveTreeDeviceMismatch` – stub `fstat` device for one directory; same expectations.
- `TestRemoveTreeFailsClosedWithoutOpenat2` – opener returns `ENOSYS`; nothing is removed.
- `TestRemoveTreeSymlinkSwap` – a directory replaced by a symlink to an outside directory between listing
  and open (hook in the walk); the outside directory is untouched.
- `TestRemoveTreeReadOnlyDirs` – 0500 directories are removed (replaces the `chmodTree` cases).
- `TestRemoveTreeBudget` – a budget of N entries stops the walk; a second call finishes it.
- `TestRemoveTreeRealMount` – real kernel behaviour: the test re-executes itself in a new user and mount
  namespace, mounts a tmpfs and a bind mount inside a tree, runs `removeTree`, and checks both mounts'
  content. Runs only with `AGENT_JOBS_TEST_MOUNTNS=1` and skips when the namespace cannot be created.
  It cannot run inside a job (the guard forbids namespaces), so the orchestrator runs it on the host.
- `TestPruneTmpKeepsMountedSubtree` (`prune_tmp_test.go`) – end to end through `PruneTmpContext` with the
  stubbed opener: the job is reported with an error, the rest of `tmp/` is gone.

Acceptance:

```
go test ./internal/jobs -count=1 -run 'TestRemoveTree|TestPruneTmp|TestPrune' 
AGENT_JOBS_TEST_MOUNTNS=1 go test ./internal/jobs -count=1 -run TestRemoveTreeRealMount -v   # host, outside a job
grep -n 'os.RemoveAll' internal/jobs/prune.go      # expect: no match
GOOS=darwin go vet ./internal/jobs
scripts/check.sh
```

### S2 – shared hold rule (High b). About 2 h

Files: `manager.go` (`lifecycleHold`), `prune.go:224-248`, `finish.go:720`, `finish.go:866-869`,
`resume.go:848`, `resume.go:1041`.

Failing tests first:

- `TestPruneSkipsPendingScopeStop` – a finished job past both retentions with `scope_pending = 1`: neither
  the temp pass nor the full prune removes anything, `Skipped` names the scope; after
  `SetScopePending(false)` the next pass removes the temp dir.
- `TestLifecycleHoldTable` – each of the four conditions alone yields its reason; none yields "".
- Existing cleanup and resume tests stay green unchanged (the refusal texts do not change).

Acceptance:

```
go test ./internal/jobs -count=1 -run 'TestPruneSkipsPendingScopeStop|TestLifecycleHold|TestCleanup|TestResume'
grep -n 'ScopePending' internal/jobs/prune.go internal/jobs/finish.go internal/jobs/resume.go   # expect: only doneLine/Containment in finish.go
scripts/check.sh
```

### S3 – doctor and dry run (High c, Low). About 3 h

Files: `system.go` (`Disk`, `jobDirs`, `DiskStatus`), `prune.go` (`treeSize` removed, `PruneEntry.Partial`,
`PruneReport.Partial`), `internal/mcp/tools_system.go` (problem line, field descriptions),
`cmd/agent-jobs/main.go` (dry-run output says "at least" when partial).

Failing tests first:

- `TestDiskDoesNotTakeLifecycleLock` – the test holds `tick.lock`; `Disk()` returns within 1 s (red today:
  it blocks).
- `TestDiskDoesNotWalkFinishedJobs` – a finished job's `tmp/` is made unreadable (0000) and its row records
  5 GiB; `Disk()` reports the recorded size and no error.
- `TestDiskPrunableFromRecordedSizes` – rows on each side of both cutoffs, an archived one excluded.
- `TestDiskReportsOverdueTmp` – a row past retention + 3 intervals with `dir_tmp_bytes > 0` appears in
  `PruneOverdue` and in `system_doctor` problems.
- `TestDiskAggregatesRecordedPartial` – one finished row with `dir_partial = 1` sets `job_dirs_partial`.
- `TestPruneDryRunBoundedWalk` – with `dirWalkMaxEntries = 10`, a dry run over a 1 000-file temp dir
  returns with `Partial` set.

Acceptance:

```
go test ./internal/jobs ./internal/mcp -count=1 -run 'TestDisk|TestPruneDryRun|TestSystemDoctor'
grep -n 'Prune(true)' internal/jobs/system.go      # expect: no match
grep -rn 'treeSize' internal/jobs --include='*.go' # expect: no match
scripts/check.sh
```

### S4 – records. About 1 h

`docs/PLAN.md` and `docs/PLAN.ru.md` I9 (the decision line gains: bounded remover, shared hold rule, doctor
estimate), `docs/ARCHITECTURE.md`, `docs/GUIDE.md`, `docs/GUIDE.ru.md`, `docs/REFERENCE*.md`
(`prunable_bytes` is an estimate, `prune_overdue`, the mount rule), `changelog.d/240.fixed.md`.

Acceptance: `go run ./cmd/changelog --validate && scripts/check.sh`.

### After merge (host, by the orchestrator)

```
agent-jobs prune --agent thrall --dry-run     # lists temp dirs past 2 h, returns promptly
# wait one TmpPruneInterval (10 min), then:
agent-jobs status --agent thrall              # or system_doctor: job_tmp_bytes drops, no prune_overdue
df -h ~/.agent-jobs
```

## 7. Residual risk

1. **A live same-user writer can still move a directory out of the tree while it is being emptied.** The
   walk holds a descriptor; if a surviving process renames that directory to somewhere outside `tmp/`, the
   walk finishes emptying it there. What is lost is content that was inside the temp dir when the walk
   entered it, plus anything added during the remaining seconds. Jobs run as the daemon's user, so no
   user-space remover can exclude this; it needs the OS-level isolation of I9. Likelihood is low: with a
   systemd scope the hold rule proves the cgroup empty first; the gap is real only under "session only"
   containment (macOS, containers). Same exposure as today's 48 h prune.
2. **The reverse move is by design.** Whatever a process moves *into* a finished job's `tmp/` is removed.
3. **A mount under `tmp/` blocks reclamation of that subtree** until someone unmounts it. Safe, visible
   (`prune_overdue`, prune error naming the path), not self-healing.
4. **`openat2` unavailable** (kernel before 5.6, restrictive seccomp in a container): no removal at all,
   by choice. Visible the same way. The supported Linux floor becomes 5.6 for pruning.
5. **Two hours is a delay, not a cap.** `max_running` jobs can still fill a disk inside the window. This
   design closes the accumulation that caused #240 (dozens of finished jobs × 48 h), not a single runaway
   job. The answer to that is a per-job size cap (section 5), tracked under I9.
6. **Doctor's `prunable_bytes` becomes an estimate**: it includes records prune keeps for jobs past 48 h
   that are not archived yet, and trusts sizes recorded when the job settled (lower bounds when
   `partial`). The exact figure is `agent-jobs prune --dry-run`.
7. **Non-Linux boundary is `st_dev` only.** Assumed sufficient on macOS (inferred, not tested).
8. **Trees deeper than 256 levels** are not removed (error, visible). `os.RemoveAll` had no such limit.
9. **Behaviour change at the other call sites**: archive expiry, sources and manifest cleanup now refuse a
   mounted subtree where they used to cross it. Intended, but a host that keeps the log archive behind a
   nested mount will see new errors on archive expiry; the archive root itself is opened by path and may
   be a mount, only mounts *below* a removed entry are refused.
10. **Not verified empirically**: that `os.RemoveAll` crosses a mount today, and that the opt-in
    real-mount test can create a user namespace on this host (Ubuntu may restrict unprivileged user
    namespaces). If it cannot, S1's kernel-level proof rests on the man page plus a manual run as root.
