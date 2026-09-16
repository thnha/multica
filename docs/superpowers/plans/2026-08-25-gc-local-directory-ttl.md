# GC Local-Directory TTL Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the daemon GC loop an opt-in TTL so a `local_directory` task's envRoot (its `logs/` + `output/` logbook — never the user's own checkout) is eventually fully reclaimed once the parent issue is done, instead of being retained on disk forever.

**Architecture:** Add one new `time.Duration` config field, `GCLocalDirectoryTTL`, parsed the same way every other GC TTL is (`server/internal/daemon/config.go`). Wire it into the single existing choke point that governs every `local_directory` GC decision, `applyLocalDirectoryGCOverride` (`server/internal/daemon/gc.go:417`): when the field is set (non-zero) and the task's own `completed_at` has also cleared it, let a `gcActionClean` decision through unchanged instead of demoting it to artifact-only cleanup. Default stays `0` (disabled), so behavior is unchanged until an operator opts in.

**Tech Stack:** Go daemon, `server/internal/daemon/` package, standard `testing` package (table/subtests already used throughout `gc_test.go` and `config_test.go`).

## Global Constraints

- Go conventions: `gofmt`, `go vet`, checked errors (per root `CLAUDE.md`).
- Code comments must be English (per root `CLAUDE.md`).
- No compatibility layers, fallback paths, or legacy shims for internal code beyond what's specified here (per root `CLAUDE.md`).
- Avoid broad refactors — touch only the files listed below (per root `CLAUDE.md`).
- No new dependencies.
- Verification: run `(cd server && go test ./internal/daemon/... -run TestLoadConfig -v)` and `(cd server && go test ./internal/daemon/... -run TestShouldCleanTaskDir -v)` per task; run `make test` before considering the plan done (per root `CLAUDE.md` Verification section).
- Commits: atomic, conventional prefix `feat(daemon)` (per root `CLAUDE.md` Commits section) — only if/when the user asks for a commit; do not commit unprompted.

## Background (why this plan exists)

Investigated 2026-08-25: on this machine's local daemon, `~/multica_workspaces_local/<ws-id>/` holds 364 task dirs / 2.1GB, and 309 of them (85%) carry `"local_directory":true` in `.gc_meta.json`. `applyLocalDirectoryGCOverride` (`gc.go:417-448`) unconditionally demotes `gcActionClean` → `gcActionCleanArtifacts` for every such task, so their envRoots are never fully removed no matter how long the parent issue has been `done` — confirmed against issue VIB-26 (`e8e96497-13f5-4fda-9ff5-2a1107f65a5f`, done since 2026-08-08), whose task dir `f75e0209/` still exists 17 days later. This is deliberate (see the comment block at `gc.go:421-425`: "so the user can inspect output/ and logs/ for forensic context"), but there is currently no way to bound it — the only lever, `GCCompletedTaskTTL`, explicitly excludes `local_directory` tasks (`gc.go:544-548`). This plan adds that missing, opt-in lever.

---

### Task 1: Add `GCLocalDirectoryTTL` config field

**Files:**
- Modify: `server/internal/daemon/config.go:78` (const block), `:117` (struct field), `:471` (env parsing block), `:548` (struct literal)
- Test: `server/internal/daemon/config_test.go`

**Interfaces:**
- Produces: `Config.GCLocalDirectoryTTL time.Duration` (zero value = disabled) and `DefaultGCLocalDirectoryTTL time.Duration` (= `0`), both consumed by Task 2.

- [ ] **Step 1: Write the failing test**

Add to `server/internal/daemon/config_test.go` (near `TestLoadConfig_CompletedTaskTTLDefaultsDisabledOnSelfHostAndReadsEnv`, same file already imports `path/filepath`, `strings`, `time`):

```go
func TestLoadConfig_LocalDirectoryTTLDefaultsDisabledAndReadsEnv(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	t.Setenv("MULTICA_GC_LOCAL_DIRECTORY_TTL", "")

	overrides := Overrides{
		ServerURL:      "http://localhost:0",
		WorkspacesRoot: t.TempDir(),
	}
	cfg, err := LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with default local-directory TTL: %v", err)
	}
	if cfg.GCLocalDirectoryTTL != 0 {
		t.Fatalf("GCLocalDirectoryTTL = %s, want disabled", cfg.GCLocalDirectoryTTL)
	}

	t.Setenv("MULTICA_GC_LOCAL_DIRECTORY_TTL", "720h")
	cfg, err = LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with local-directory TTL: %v", err)
	}
	if cfg.GCLocalDirectoryTTL != 720*time.Hour {
		t.Fatalf("GCLocalDirectoryTTL = %s, want 720h", cfg.GCLocalDirectoryTTL)
	}

	t.Setenv("MULTICA_GC_LOCAL_DIRECTORY_TTL", "not-a-duration")
	if _, err := LoadConfig(overrides); err == nil || !strings.Contains(err.Error(), "MULTICA_GC_LOCAL_DIRECTORY_TTL") {
		t.Fatalf("LoadConfig invalid local-directory TTL error = %v, want named validation error", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `(cd server && go test ./internal/daemon/... -run TestLoadConfig_LocalDirectoryTTL -v)`
Expected: FAIL — `cfg.GCLocalDirectoryTTL undefined (type Config has no field or method GCLocalDirectoryTTL)`

- [ ] **Step 3: Add the default constant**

In `server/internal/daemon/config.go`, in the `const (...)` block, directly after the existing `DefaultGCRepoTTL` line (`config.go:78`):

```go
	DefaultGCRepoTTL = 30 * 24 * time.Hour // 30 days — evict a bare repo cache no task has checked out this long
	// DefaultGCLocalDirectoryTTL is 0 — disabled. A local_directory task's
	// envRoot holds only the daemon's logbook (logs/, output/), never a git
	// checkout, so full removal here is lower-risk than GCCompletedTaskTTL's
	// full removal — but it is still a behavior change from "kept forever" to
	// "eventually gone", so it stays opt-in until an operator sets
	// MULTICA_GC_LOCAL_DIRECTORY_TTL themselves.
	DefaultGCLocalDirectoryTTL = time.Duration(0)
```

- [ ] **Step 4: Add the struct field**

In `server/internal/daemon/config.go`, in `type Config struct`, directly after the existing `GCCompletedTaskTTL` field (`config.go:117`):

```go
	GCCompletedTaskTTL             time.Duration         // fully clean inactive issue-task envs completed at least this long ago, regardless of parent issue status (default: 14d on Multica Cloud, 0/disabled elsewhere; local_directory envs never fully leave via this TTL — see GCLocalDirectoryTTL)
	GCLocalDirectoryTTL            time.Duration         // once a local_directory task's parent issue is done/cancelled and past GCTTL, also require the task's own completed_at to be at least this old before fully removing its envRoot (logs/ and output/) instead of the default artifact-only policy; 0 disables and keeps today's behavior (default: 0/disabled — see DefaultGCLocalDirectoryTTL)
```

(This also edits the existing `GCCompletedTaskTTL` comment: replace `"; local_directory envs are never fully removed)"` with `"; local_directory envs never fully leave via this TTL — see GCLocalDirectoryTTL)"`.)

- [ ] **Step 5: Add env parsing**

In `server/internal/daemon/config.go`, directly after the existing `gcCompletedTaskTTL` parsing block (`config.go:468-471`):

```go
	gcCompletedTaskTTL, err := durationFromEnv("MULTICA_GC_COMPLETED_TASK_TTL", defaultGCCompletedTaskTTL(serverBaseURL))
	if err != nil {
		return Config{}, err
	}
	gcLocalDirectoryTTL, err := durationFromEnv("MULTICA_GC_LOCAL_DIRECTORY_TTL", DefaultGCLocalDirectoryTTL)
	if err != nil {
		return Config{}, err
	}
```

- [ ] **Step 6: Wire it into the returned `Config{}` literal**

In `server/internal/daemon/config.go`, directly after the existing `GCCompletedTaskTTL:` line in the `return Config{...}` block (`config.go:548`):

```go
		GCCompletedTaskTTL:              gcCompletedTaskTTL,
		GCLocalDirectoryTTL:             gcLocalDirectoryTTL,
```

- [ ] **Step 7: Run test to verify it passes**

Run: `(cd server && go test ./internal/daemon/... -run TestLoadConfig_LocalDirectoryTTL -v)`
Expected: PASS

- [ ] **Step 8: Run the full config test file to check for regressions**

Run: `(cd server && go test ./internal/daemon/... -run TestLoadConfig -v)`
Expected: PASS (all `TestLoadConfig_*` cases, including the untouched `CompletedTaskTTL` ones)

- [ ] **Step 9: Commit**

```bash
git add server/internal/daemon/config.go server/internal/daemon/config_test.go
git commit -m "feat(daemon): add opt-in GCLocalDirectoryTTL config field"
```

---

### Task 2: Wire `GCLocalDirectoryTTL` into `applyLocalDirectoryGCOverride`

**Files:**
- Modify: `server/internal/daemon/gc.go:392-448` (`applyLocalDirectoryGCOverride`, plus a new helper)
- Test: `server/internal/daemon/gc_test.go`

**Interfaces:**
- Consumes: `Config.GCLocalDirectoryTTL` (Task 1), `execenv.GCMeta.CompletedAt time.Time`, `execenv.GCMeta.LocalDirectory bool` (both already exist), `gcAction` constants `gcActionClean` / `gcActionOrphan` / `gcActionSkip` (already exist in `gc.go:337-343`).
- Produces: `(d *Daemon) localDirectoryTTLExceeded(meta *execenv.GCMeta) bool`, used only inside `applyLocalDirectoryGCOverride`.

- [ ] **Step 1: Write the failing tests**

Add to `server/internal/daemon/gc_test.go`, directly after the existing `TestShouldCleanTaskDir_LocalDirectoryNeverClean` (`gc_test.go:2074-2109`) — that test is left unchanged, since it exercises the still-correct default (`GCLocalDirectoryTTL` unset = `0`):

```go
// TestShouldCleanTaskDir_LocalDirectoryTTLReclaimsAfterBothTTLsElapse confirms
// that once GCLocalDirectoryTTL is set, and both the parent issue (GCTTL) and
// the task's own completed_at (GCLocalDirectoryTTL) are past their TTLs, a
// local_directory task's envRoot is fully reclaimed like any other issue task.
func TestShouldCleanTaskDir_LocalDirectoryTTLReclaimsAfterBothTTLsElapse(t *testing.T) {
	t.Parallel()
	issueID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa4"

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/daemon/issues/%s/gc-check", issueID), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":     "done",
			"updated_at": time.Now().Add(-60 * 24 * time.Hour),
		})
	})

	d := newGCTestDaemon(t, mux)
	d.cfg.GCLocalDirectoryTTL = 30 * 24 * time.Hour
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "local-task-old", &execenv.GCMeta{
		Kind:           execenv.GCKindIssue,
		IssueID:        issueID,
		WorkspaceID:    "ws1",
		CompletedAt:    time.Now().Add(-60 * 24 * time.Hour),
		LocalDirectory: true,
	})

	if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionClean {
		t.Fatalf("expected gcActionClean once both GCTTL and GCLocalDirectoryTTL have elapsed, got %d", got)
	}
}

// TestShouldCleanTaskDir_LocalDirectoryTTLStaysArtifactOnlyBeforeItsOwnTTL
// confirms the parent-issue TTL alone is not enough: the task's own
// completed_at must also clear GCLocalDirectoryTTL before full removal.
func TestShouldCleanTaskDir_LocalDirectoryTTLStaysArtifactOnlyBeforeItsOwnTTL(t *testing.T) {
	t.Parallel()
	issueID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa5"

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/daemon/issues/%s/gc-check", issueID), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":     "done",
			"updated_at": time.Now().Add(-10 * 24 * time.Hour), // past the 5d GCTTL used by newGCTestDaemon
		})
	})

	d := newGCTestDaemon(t, mux)
	d.cfg.GCLocalDirectoryTTL = 30 * 24 * time.Hour
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "local-task-recent", &execenv.GCMeta{
		Kind:           execenv.GCKindIssue,
		IssueID:        issueID,
		WorkspaceID:    "ws1",
		CompletedAt:    time.Now().Add(-10 * 24 * time.Hour), // NOT past the 30d GCLocalDirectoryTTL yet
		LocalDirectory: true,
	})

	if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionCleanArtifacts {
		t.Fatalf("expected gcActionCleanArtifacts while local_directory task is under its own TTL, got %d", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `(cd server && go test ./internal/daemon/... -run TestShouldCleanTaskDir_LocalDirectoryTTL -v)`
Expected: FAIL — `TestShouldCleanTaskDir_LocalDirectoryTTLReclaimsAfterBothTTLsElapse` fails because `got` is `gcActionCleanArtifacts`, not `gcActionClean` (current code always demotes).

- [ ] **Step 3: Implement the override + helper**

In `server/internal/daemon/gc.go`, replace the body of `applyLocalDirectoryGCOverride` (`gc.go:417-448`):

```go
func (d *Daemon) applyLocalDirectoryGCOverride(meta *execenv.GCMeta, action gcAction) gcAction {
	if !meta.LocalDirectory {
		return action
	}
	// A confirmed-done parent past GCTTL already earned gcActionClean before
	// this override ran (gcDecisionIssueResult). Once the task's own
	// completed_at is also past the dedicated local_directory TTL, let that
	// full cleanup through instead of demoting it below — otherwise a
	// local_directory env would never leave disk. The orphan path (parent
	// inaccessible, never confirmed done) is deliberately excluded: an
	// unreachable parent record is not the same signal as a confirmed-done
	// one, so it keeps the existing managed-artifact-only policy below.
	if action == gcActionClean && d.localDirectoryTTLExceeded(meta) {
		return action
	}
	// local_directory tasks keep their envRoot indefinitely so the user
	// can inspect output/ and logs/ for forensic context. The WorkDir is
	// the user's own path and lives outside taskDir. The envRoot contains
	// the daemon's durable logbook plus regenerable tool caches, so keep the
	// former while allowing narrowly scoped cleanup of the latter.
	//
	//   gcActionClean   → demote to artifact-pattern cleanup so envRoot
	//                     (and especially the logbook) survives, unless
	//                     GCLocalDirectoryTTL has also elapsed (above).
	//   gcActionOrphan  → exact managed-artifact cleanup only; we don't ever
	//                     wipe a local_directory envRoot via the mtime path,
	//                     since the parent issue / chat record going away
	//                     should not collateral-delete the user's audit trail.
	//
	// Artifact cleanup remains disabled when GCArtifactTTL is explicitly zero.
	// gcActionCleanArtifacts, gcActionCleanManagedArtifacts, and gcActionSkip obey the
	// "no full envRoot RemoveAll" rule.
	if d.cfg.GCArtifactTTL <= 0 {
		return gcActionSkip
	}
	switch action {
	case gcActionClean:
		return gcActionCleanArtifacts
	case gcActionOrphan:
		return gcActionCleanManagedArtifacts
	default:
		return action
	}
}

// localDirectoryTTLExceeded reports whether a local_directory task's own
// completed_at is old enough to earn full envRoot removal under
// GCLocalDirectoryTTL. A zero CompletedAt (task never reported completion
// through WriteGCMeta) is left to the existing artifact-only policy rather
// than guessed from an unrelated clock, matching applyManagedArtifactFallback's
// handling of the same case.
func (d *Daemon) localDirectoryTTLExceeded(meta *execenv.GCMeta) bool {
	if d.cfg.GCLocalDirectoryTTL <= 0 || meta.CompletedAt.IsZero() {
		return false
	}
	return time.Since(meta.CompletedAt) > d.cfg.GCLocalDirectoryTTL
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `(cd server && go test ./internal/daemon/... -run TestShouldCleanTaskDir_LocalDirectoryTTL -v)`
Expected: PASS

- [ ] **Step 5: Run the full local_directory + GC test suite to check for regressions**

Run: `(cd server && go test ./internal/daemon/... -run 'TestShouldCleanTaskDir|TestGC' -v)`
Expected: PASS, including the untouched `TestShouldCleanTaskDir_LocalDirectoryNeverClean`, `TestShouldCleanTaskDir_LocalDirectoryOrphanUsesManagedOnly`, `TestShouldCleanTaskDir_LocalDirectoryFalsePreservesNormalClean`, and `TestShouldCleanTaskDir_CompletedTaskTTLKeepsLocalDirectoryArtifactTTLSeparate`.

- [ ] **Step 6: Run the full daemon package test suite**

Run: `(cd server && go test ./internal/daemon/... -v)`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add server/internal/daemon/gc.go server/internal/daemon/gc_test.go
git commit -m "feat(daemon): reclaim local_directory task envRoots once GCLocalDirectoryTTL elapses"
```

---

## Rollout note (read before enabling)

`GCLocalDirectoryTTL` defaults to `0` (disabled) on every deployment after this plan — no behavior changes until an operator sets `MULTICA_GC_LOCAL_DIRECTORY_TTL` explicitly. When choosing a value to enable:

- The removed data is only `logs/` + `output/` (never a git checkout — `local_directory` tasks run in the user's own working directory, which lives outside `taskDir`), so the blast radius of getting the value wrong is much smaller than `GCCompletedTaskTTL`.
- Both TTLs must clear before removal: the parent issue must already be `done`/`cancelled` past `GCTTL` (default 24h), AND the task's own `completed_at` must be past `GCLocalDirectoryTTL`. A long-open issue whose task ran once and finished stays artifact-only forever regardless of this TTL.
- This plan does not change the `GCCompletedTaskTTL` cloud/self-host default split, and does not give `GCLocalDirectoryTTL` its own cloud-vs-self-host default — it ships disabled everywhere. If Multica Cloud wants it bounded by default the way `GCCompletedTaskTTL` is, that is a follow-up decision (add a `defaultGCLocalDirectoryTTL(serverBaseURL)` helper mirroring `defaultGCCompletedTaskTTL`), not part of this plan.
