Korean: [usage.md](./usage.md)

# gno-perf usage

Collect GnoVM performance metrics for gno-core commits, store them in SQLite, and compare two commits for changed series. Tier 1 is primary; Tier 2 is informational. See [SPEC.md](../SPEC.md) for the design.

## Metrics

| Tier | kind | Metrics | Change criteria |
|---|---|---|---|
| 1 (deterministic) | `unit` | `gas`, `cycles`, `allocs` | `Δ != 0`; exclude `cycles`/`allocs` within rounding error |
| | `pkg` | `storage` | `Δ != 0` |
| | `filetest` | `gas`, `storage` | `Δ != 0` |
| | `tx` (txtar) | `gas_used` | `Δ != 0` |
| | `bin` | `bytes` (`gno`, `gnoland`, `gnokey`) | `\|Δ%\| >= 0.5`; exclude if Go version or OS/arch differs |
| 2 (wall-clock) | `gobench` | `ns_op`, `b_op`, `allocs_op` | benchstat-style `p < 0.05` and `\|Δ%\| >= 5`; same host and at least 10 samples on each side |
| | `wall` | Per-package `seconds` | Stored only; one sample, so excluded from reports |

## Setup

- Go 1.25.9 (`mise.toml`). Each target commit runs with `GOTOOLCHAIN` pinned to its `go.mod` version.
- A full-history gno-core clone; for a shallow clone, run `git fetch --unshallow`.
- `git`, `nice`, and authenticated `gh` for `pr`.
- Network access: PRs and master are fetched directly from `https://github.com/gnolang/gno.git`.

```sh
mise run build        # = go build -o bin/gnoperf ./cmd/gnoperf
mise run check        # fmt, vet, test, build
```

Configuration defaults to `gnoperf.toml` in the current directory; override with `--config <path>`. Relative paths resolve from the config file; `~/` means the home directory. Unknown keys are errors.

| Key | Meaning |
|---|---|
| `gno_core` | gno-core clone path (required) |
| `cache_dir` | DB, reports, and logs (required) |
| `worktree_dir` | Collection worktree; must differ from `gno_core` (required) |
| `parallelism` | Default `gno test -p`; at least 1 |
| `quick_packages` | `examples/` packages for `--quick` |
| `tier2.count` | Go benchmark `-count`; at least 10 |
| `tier2.vm_bench` | Benchmark regex for `gnovm/pkg/gnolang` |
| `tier2.store_packages` | Store benchmark packages |
| `tier2.store_bench` | Benchmark regex for those packages |

## Commands

```
gnoperf [--config path] <command> [options]
```

### Shared collection options

For `collect`, `pr`, `sync`, `backfill`, and `bisect`:

| Option | Effect |
|---|---|
| `--pkgs a,b` | Limit Tier 1 to specified `examples/` packages, e.g. `gno.land/p/nt/avl/...` |
| `--quick` | Run only `quick_packages`; incompatible with `--pkgs` |
| `--skip-tx` | Skip the txtar suite |
| `--skip-tier2` | Skip Go benchmarks |
| `--no-repeat` | Skip the determinism check (second Tier 1 run) |
| `--force` | Ignore the cache and collect again |
| `-p N` | `gno test -p` parallelism; defaults to `parallelism` |

A **complete** collection is reused only when sha, harness version, scope, host, and Go version all match.

### `pr`: collect and compare a PR

```sh
gnoperf pr 6089
gnoperf pr 6476 --quick --skip-tx --skip-tier2
gnoperf pr 6476 --rebase-base
```

| Option | Effect |
|---|---|
| `--base <sha>` / `--head <sha>` | Override the automatic base/head |
| `--rebase-base` | Compare master with a temporary commit merging the PR onto its tip; abort on conflicts; incompatible with `--base` |
| `--all` | Show every series without package grouping |

- Merged PR: head = merge commit, base = `head^1`. Open PR: head = `headRefOid`, base = merge-base with the base branch.
- Base runs once; head runs twice for determinism. PR reports always warn `determinism check was skipped`.
- Report: `<cache_dir>/reports/pr-<n>-<head7>.md` and stdout.

### `collect`: collect one commit

```sh
gnoperf collect            # HEAD of gno_core
gnoperf collect d0cbe58 --quick
```

Output: `collection <id>: <sha>, <N> measurements, <K> nondeterministic series, <seconds>s`. A failed step saves an incomplete collection and returns an error with the log path.

### `compare`: compare stored collections

```sh
gnoperf compare 18f41ec d0cbe58
gnoperf compare 18f41ec d0cbe58 --format text --all
```

Both commits must already be in the DB; uses each commit's latest collection. No collection or report file is created. Only options: `--format md|text` (default `md`) and `--all`.

### `sync`: collect new master commits for cron

```sh
gnoperf sync
gnoperf sync --max 3 --quick --skip-tx --skip-tier2
```

- Fetch master, follow `--first-parent`, and compare each commit with its first parent.
- Progress: `<cache_dir>/sync-cursor`. **First run (no cursor): only the master tip**, or the latest N commits with `--max N`. Use `backfill` for older commits.
- `--max N`: maximum commits per run; 0 means unlimited.
- If `sync.lock` is held, print `sync already running; skipped` and exit.
- Incomplete collections still produce reports with warnings; processing continues.
- Reports: `<cache_dir>/reports/<sha>.md`. Cron/systemd examples: `examples/`.

### `backfill`: collect a historical range

```sh
gnoperf backfill origin/master~100..origin/master
gnoperf backfill <from>..<to> --every 10
```

Compare first-parent commits in `from..to` (excluding `from`) with their parents. `--every N` collects every Nth commit. Reports use the `sync` location; the sync cursor is unchanged.

### `bisect`: find a Tier 1 series change

```sh
gnoperf bisect <good> <bad> --series 'gno.land/p/nt/avl/v0#TestTreeSet:gas' --quick
```

- `--series <benchmark>:<metric>` uses the report's benchmark column. Expand `(N tests)` groups with `--all`.
- Requires a deterministic Tier 1 series, `good` on the first-parent chain of `bad`, and different endpoint values.
- Runs `git bisect run --first-parent` in a separate temporary worktree; prints the sha where the value first changed. Ignores `--no-repeat`: determinism is always checked.

### `bench ops`: run opcode benchmarks manually

```sh
gnoperf bench ops
```

Runs `BenchmarkOp*` at the HEAD of `gno_core` with `-count=3 -benchtime=2s`. Prints raw output and a summary (median ± range). No DB writes or options. Takes a long time.

## Reports

```
## gno-perf — PR #6089 (merged)
head d0cbe58c32b8 vs base 18f41ec14ecd · harness <gno-perf commit>+<binary hash>
WARNING: ...
### Regressions | Improvements | Explained changes | Tier 2 (informational)
| benchmark | metric | base | head | Δ | Δ% | size | notes |
```

| Section | Meaning |
|---|---|
| Regressions | Tier 1 increases without cause tags; review required |
| Improvements | Tier 1 decreases without cause tags; review required |
| Explained changes | Tier 1 with at least one cause tag in `notes` |
| Tier 2 (informational) | Significant wall-clock changes; benchstat statistics in `notes`; not an alert criterion |

Sorted by descending `|Δ%|`. `unit` series sharing package, metric, direction, and tags collapse into `pkg (N tests)`; expand with `--all`.

Size by `|Δ%|`: `<0.2` very small, `<1` small, `<2` moderate, `<5` large, otherwise very large.

### Cause tags

| Tag | Condition in code |
|---|---|
| `test-changed` | Content hash of the test/filetest/txtar file changed |
| `gaswanted-only` | Only txtar `-gas-wanted` / `GAS WANTED` values changed |
| `dep-changed` | `.gno`/`gnomod.toml` changed in the package or any imported `examples/` package, transitively; excludes `_test.gno`/`_filetest.gno` |
| `stdlib-changed` | A file under `gnovm/stdlibs/` or `gnovm/tests/stdlibs/` changed |
| `gas-schedule` | `gnovm/pkg/gnolang/machine.go` or `alloc.go` changed; whole-file check, not just constant blocks |

### Warnings

| Warning | Meaning / action |
|---|---|
| `one or both collections are incomplete` | Step failure; check subsequent `<sha> <step>: ...` lines and logs |
| `collection scopes differ` | Different scopes; only overlapping series compared |
| `collector versions differ` | Different gno-perf binaries; recollect with the current binary and compare |
| `determinism check was skipped` | At least one side used `--no-repeat`; always present for `pr` (base) |
| `hosts differ; wall-clock series are excluded` | No Tier 2 comparison |
| `Go toolchains differ; binary-size series are excluded` | No `bin` comparison |
| `<sha>: N nondeterministic series excluded` | Values differed between runs; series excluded |
| `N added or removed series have no counterpart` | Series present on only one side |

## Output paths (relative to `cache_dir`)

| Path | Contents |
|---|---|
| `perf.db` | SQLite DB (`artifact`, `collection`, `series`, `value`, `error`) |
| `reports/pr-<n>-<head7>.md`, `reports/<sha>.md` | Reports |
| `logs/<sha>/<time>/<step>.log.gz` | Raw output per step (`build`, `features`, `t1`, `t1-repeat`, `tx`, `gobench-0`, `gobench-1`) |
| `logs/<sha>/<time>/collection.json` | Full collection: values, errors, nondeterministic series |
| `bin/<sha>/` | Built `gno`, `gnoland`, `gnokey` binaries |
| `sync-cursor`, `sync.lock`, `bisect.lock` | sync/bisect state |

Worktree lock: `<worktree_dir>.lock`. Only one collection runs at a time.
