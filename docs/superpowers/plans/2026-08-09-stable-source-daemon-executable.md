# Stable Source Daemon Executable Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ensure `make daemon` starts the background daemon from a persistent executable instead of a `go run` temporary binary that disappears after the launcher exits.

**Architecture:** Keep the general source CLI target unchanged. Give the daemon-specific target a stable lifecycle by building `server/bin/multica` with the existing version metadata and invoking that binary for `daemon restart --profile local`.

**Tech Stack:** GNU Make, Go toolchain, shell regression test.

## Global Constraints

- Make a small, focused change.
- Add no dependencies.
- Do not change installed CLI daemon behavior.
- Do not commit unless explicitly requested.

---

### Task 1: Launch the source daemon from a persistent binary

**Files:**
- Create: `scripts/test-make-daemon-command.sh`
- Modify: `Makefile:260-268`

**Interfaces:**
- Consumes: existing `VERSION`, `COMMIT`, and `DATE` Make variables.
- Produces: `make daemon` builds `server/bin/multica` and invokes it with `daemon restart --profile local`.

- [ ] **Step 1: Write the failing test**

Create a shell test that runs `make -n daemon`, asserts it includes `go build` output targeting `bin/multica`, asserts the stable binary is invoked with `daemon restart --profile local`, and rejects `go run ... ./cmd/multica` in the daemon recipe expansion.

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash scripts/test-make-daemon-command.sh`

Expected: FAIL because the current daemon target delegates to `multica`, which uses `go run`.

- [ ] **Step 3: Write the minimal implementation**

Change only the `daemon` recipe to build `server/bin/multica` with the same ldflags as the `multica` target and then execute `server/bin/multica daemon restart --profile local`.

- [ ] **Step 4: Run focused verification**

Run: `bash scripts/test-make-daemon-command.sh`

Expected: PASS.

Run: `make -n daemon`

Expected: the expansion builds and launches `server/bin/multica`; it contains no `go run` command.

- [ ] **Step 5: Check the patch**

Run: `git diff --check -- Makefile scripts/test-make-daemon-command.sh docs/superpowers/plans/2026-08-09-stable-source-daemon-executable.md`

Expected: PASS with no output.
