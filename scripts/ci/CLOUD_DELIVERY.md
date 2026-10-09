# Cloud delivery pipeline

An explicit HotPlex Issue becomes a scoped Codex change, locally validated
contents, and a draft PR. Existing Git hooks and GitHub CI remain mandatory.
The pipeline never merges or deploys, and does not restore automatic review CI.

## Environment

On the prepared cloud computer:

```bash
source /workspace/cloud-setup/optimized.sh
cloud-doctor
golangci-lint --version
codex login status
gh api repos/hrygo/hotplex --jq .permissions.push
```

Use the project-pinned Go and pnpm versions and golangci-lint v2.11.4, matching
CI. Run one delivery per checkout. The pipeline uses the existing checkout and
refuses to overwrite uncommitted changes; it does not create worktrees. Commands
run sequentially under the cloud environment's four-worker build defaults.

The prepared cloud computer has a `hotplex-deliver` wrapper backed by a supervisor
copy outside the Git checkout, at
`/workspace/cloud-setup/hotplex-delivery/cloud_delivery.py`. This is important:
preparation switches the checkout to the base branch, which may not yet contain
this new script. Keep the supervisor outside the task checkout when installing
elsewhere, and invoke that copy with `--repo /path/to/hotplex`.

## Run a task

Create an open GitHub Issue with requirements and observable acceptance criteria.
Then, from a clean checkout:

```bash
hotplex-deliver run --issue ISSUE_NUMBER
```

An optional `--task-file /absolute/path/task.txt` supplies a more detailed local
brief. The Issue title and reference still identify the delivery. Task text is
passed on stdin to `codex exec`, never interpolated into shell code.

The command prints a state-file path. Evidence is stored outside the checkout
under `~/.local/state/hotplex-delivery/`. Treat logs and briefs as private local
artifacts; they are not uploaded into the PR. Codex runs with the workspace-write
sandbox and uses the configured authentication, skills and MCP tools. It does
not receive permission to bypass hook trust. Review installed hooks through
Codex `/hooks` if required. This is intended for trusted, explicitly selected
tasks in the externally isolated cloud environment, not arbitrary public Issues.

## Staged execution and recovery

Each stage can run independently:

```bash
hotplex-deliver prepare --issue ISSUE_NUMBER
hotplex-deliver implement --state /absolute/path/state.json
hotplex-deliver validate --state /absolute/path/state.json
hotplex-deliver publish --state /absolute/path/state.json
hotplex-deliver status --state /absolute/path/state.json
```

After a failed implementation or gate, inspect the retained checkout and logs,
fix the problem, and rerun `validate`. `implement` deliberately refuses to restart
over partially completed work. A failed push/PR creation can be retried with
`publish`; committed/pushed checkpoints are retained and existing PRs reused.
No reset, forced push, or automatic retry of model edits is performed.

`codex login status` alone does not establish a usable session. An expired token
can still appear logged in and fail the actual model request with HTTP 401. In
that case implementation stops, logs remain available, and authentication must
be repaired before another task can be implemented automatically.

## Quality gates

The local gates run sequentially and stop at the first failure:

1. Python CI-helper tests and Git whitespace checks.
2. Go module verification.
3. Frozen pnpm installation, webchat tests and a fresh embedded webchat build.
4. Documentation build, Go vet, the CI-matching linter and Go build.
5. Full short-mode Go tests with race detection, random ordering and no test cache.

The fingerprint covers HEAD and the actual tracked/untracked file contents,
symlinks and modes, excluding ignored build caches. Changed contents after or
during validation invalidate delivery. Commit hooks must preserve validated
contents. The original pre-push hook also runs before publication. Full integration
and protocol gates remain in the existing GitHub CI; a local pass is not an
approval to merge. External-provider tests still require their real configuration.

Each command defaults to a 30-minute limit (`--timeout SECONDS` overrides it).
A timeout terminates the command process group and preserves evidence. A checkout
lock prevents two cooperating deliveries from changing the same checkout.
Do not manually edit the checkout while a run is active.

## Scheduling

This runner processes one selected Issue per invocation. It is not an unattended
issue scanner or a periodic service. Only schedule invocations after cloud uptime,
credential availability and the task-selection policy are established. The cloud
environment's sleep and persistence rules are unchanged.
