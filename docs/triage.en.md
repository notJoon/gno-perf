Korean: [triage.md](./triage.md)

# gno-perf triage

Based on [rustc-perf weekly triage](https://github.com/rust-lang/rustc-perf/tree/master/triage). Tier 1 is deterministic: check **cause classification**, rather than noise. See [usage.en.md](./usage.en.md) for commands and report format.

## 1. Before merge: measure PRs

Run `gnoperf pr <n>` only for PRs touching these paths:

| Measure | Usually skip |
|---|---|
| `gnovm/`, `tm2/`, `gno.land/pkg/` | Only `examples/` changes (only `test-changed`/`dep-changed` results) |
| Gas constants (`gnovm/pkg/gnolang/machine.go`, `alloc.go`) | Docs, CI, `misc/` |

```sh
gh pr diff <n> --repo gnolang/gno --name-only   # check changed paths
gnoperf pr <n> --quick --skip-tier2              # local Mac: start narrow
gnoperf pr <n>                                    # full run if needed
```

Do not trust Tier 2 on a local Mac; use `--skip-tier2`. The PR warning `determinism check was skipped` is expected: base runs once.

## 2. After merge: enable `sync` on a dedicated machine

Complete these steps once before enabling sync:

1. **Verify Tier 1 across machines.** Recollect a Mac-measured commit on the new machine with identical options. The different host prevents cache reuse. Copy the Mac's `perf.db` into the new machine's `cache_dir` first to compare both collections in one DB.

   ```sh
   gnoperf collect <sha> --quick --skip-tier2     # same options as on the Mac
   sqlite3 ~/.cache/gno-perf/perf.db \
     "SELECT c.id, c.host, c.harness_sha FROM collection c JOIN artifact a ON a.id=c.artifact_id WHERE a.sha='<full sha>';"
   sqlite3 ~/.cache/gno-perf/perf.db "
     SELECT s.kind, s.benchmark, s.metric, x.value, y.value
     FROM value x JOIN value y ON x.series_id=y.series_id AND x.sample=y.sample
     JOIN series s ON s.id=x.series_id
     WHERE x.collection_id=<mac id> AND y.collection_id=<new id>
       AND s.tier=1 AND s.kind!='bin' AND x.value IS NOT y.value;"
   ```

   Expect no rows. Exclude `bin` because OS/arch affects size. Resolve any differences before enabling sync: they violate the assumption in SPEC §9.2.

2. **Backfill the latest 100 commits as a baseline.** The first sync collects only the master tip; backfill the preceding history.

   ```sh
   git -C <gno_core> fetch origin
   gnoperf backfill origin/master~100..origin/master
   ```

3. **Schedule sync.** Adjust paths and install `examples/gnoperf.cron` or `gnoperf.service`/`gnoperf.timer` (every 30 minutes).

## 3. Weekly triage

Generate the week's draft, then judge only its `Review needed` entries (see §6):

```sh
gnoperf triage <last log's final commit>..origin/master > triage/YYYY-MM-DD.md
```

For a single sync report, read its `Overall result` line first: `review needed` means primary Regressions or Improvements exist. Review only those sections; skim Explained changes and the collapsed Secondary/Tier 2 blocks. The summary table counts changes below 0.2% too, so a broad shift (e.g. most txtar `gas_used` up) shows there even when no row is listed.

```sh
cd ~/.cache/gno-perf/reports
find . -name '*.md' ! -name 'pr-*' -mtime -7 -exec grep -l 'review needed' {} +
find . -name '*.md' ! -name 'pr-*' -mtime -7 -exec grep -H '^WARNING:' {} +
```

| Verdict | Criterion | Follow-up |
|---|---|---|
| Intended change | Explained by PR description or diff, e.g. more gas for a new feature | Record only |
| Actual regression | Unexplained increase | Notify the PR author or open an issue; follow §5 for possible security implications |
| Classifier error | Missing cause tag, e.g. a dependency outside the import graph | Fix classification in `compare.go` and add a regression test |
| Unclear range | Multiple commits involved, e.g. `backfill --every N` intervals | `gnoperf bisect <good> <bad> --series '<benchmark>:<metric>'` |

- Apply the same criteria to improvements: unexplained decreases may indicate missing measurements or a changed test path.
- Expand `pkg (N tests)` groups and hidden small changes with `gnoperf compare <base> <head> --all` before judging.
- A `Context:` tag such as `gas-schedule` explains no row by itself; tie each listed change to the diff.

## 4. Nondeterministic series

Treat these as **bug signals** (e.g. map iteration order or GC timing): Tier 1 values differed for the same commit and toolchain.

```sh
sqlite3 ~/.cache/gno-perf/perf.db "
  SELECT DISTINCT s.kind, s.benchmark, s.metric
  FROM value v JOIN series s ON s.id=v.series_id JOIN collection c ON c.id=v.collection_id
  WHERE v.nondeterministic=1 AND c.started_at >= date('now','-7 days');"
```

- New series: compare `logs/<sha>/<time>/t1.log.gz` and `t1-repeat.log.gz`, document reproduction conditions, and open a gno-core issue.
- Previously reported series: record only the issue link in the triage log.
- Determinism checks cover only `gno test` output (unit/pkg/filetest). txtar (`tx`) runs once and is not checked.

## 5. Rules

- **Trust Tier 2 only from one dedicated machine.** Different host strings (OS/arch/CPU) automatically exclude it from reports.
- **Recollect the baseline when the harness changes.** Its version is the gno-perf commit + binary hash; even rebuilding changes it. For `collector versions differ`, rerun `collect` for both commits with the current binary, then compare. On the dedicated machine, rebuild only when replacing the binary; record the replacement date in the triage log.
- **Publish only reviewed triage logs.** Weekly logs are committed to a public repo, but never automatically: a regression may expose a security issue such as a gas or storage bypass. Before committing, resolve the log's `Security check: TODO` line. Anything that could be a security issue goes to gno-core `SECURITY.md` (gno-security@newtendermint.org) first; leave it out of the log, or record it only as "under security review", until the fix is public. Raw reports and `perf.db` stay local. The same rule applies to public issues and PR comments.

## 6. Weekly log

Like rustc-perf's `triage/` logs: the tool writes the tables, a person writes the verdicts.

1. Take `from` from the last committed log's revision range (its final commit); run `gnoperf triage from..origin/master > triage/YYYY-MM-DD.md`.
2. Commits under `Not collected` need `sync` or `backfill` first; rerun the draft afterwards.
3. Replace each `Verdict: TODO` with a §3 verdict and a one-line reason (e.g. "Intended change: new storage deposit path, author confirmed in #NNNN"). Check every `WARNING:` before trusting `No action needed`.
4. Fill in the nondeterministic table's `Status` per §4.
5. Replace `TODO: Summary` with one to three sentences on the week, `@???` with your handle, and resolve `Security check: TODO` per §5.
6. Commit the log. Nothing else from `cache_dir` is committed.
