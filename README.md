# git-spdx

License-scan every blob in a git repository's object store and report where
detected SPDX expressions changed across the commit graph.

Uses `github.com/git-pkgs/licenses` for detection and shells out to
`git cat-file --batch` for object reading. Detections are cached by blob SHA,
so each unique piece of content is matched once regardless of how many
commits or paths reference it.

## Commands

```bash
git spdx scan [repo]   # match every blob, print throughput and expression histogram
git spdx log  [repo]   # summarise detected changes by path group
git spdx -details log [repo]  # include individual commits and paths
git spdx -group legal -details log [repo]
git spdx -monthly log [repo] > monthly.csv
```

Drop the `git-spdx` binary on `$PATH` to use it as a git subcommand.
Options go before the command.

`log` groups paths as `root` (root legal files), `legal` (legal files in
subdirectories), and `other`. Legal filenames and directories use
`licenses.LegalFileRoles`; a subdirectory does not establish that a file
is vendored. Use `-group root`, `-group legal`, or `-group other` to filter
the report.

The report counts file additions and deletions separately from expression
changes in existing files. It also counts files gaining or losing a parsed
SPDX declaration, including when the detected expression stays the same.
These are changes in detected evidence, which can include corrections or
new declarations of existing terms. They do not establish relicensing.
Skipped blobs produce incomplete comparisons, never inferred license
additions or removals. Repeated notices with the same expression do not
produce expression changes.

`-monthly` writes CSV grouped by author month and path group. Commit counts
include reported changes and incomplete comparisons. They are distinct
within each group; a commit touching several groups appears in each.
The SPDX columns count files gaining or losing their first/last
parsed declaration, including file additions and deletions.

The default text limit is 1 MiB, raised to 8 MiB for any blob that appears
at a legal path in reachable history. Every historical path occurrence
contributes to eligibility, including deleted legal files and shared blobs.
Set `-max-blob-size` and `-max-legal-blob-size` in bytes to change these
limits; legal blobs use the larger value. A zero limit permits only empty
blobs. Size, binary, and matcher-error skips are reported separately.

## Numbers

M1 Pro, 8 cores, 16 GB, go1.26.7.

| repo | commits | blobs | scan wall | peak RSS | log wall |
|---|---|---|---|---|---|
| rust-lang/cargo | 23,052 | 57,529 | 6.1 s | 306 MB | - |
| rubygems/rubygems | 48,222 | 91,478 | 4.4 s | 476 MB | 8.1 s |
| homebrew-core | 828,252 | 708,799 | 17.6 s | 1.30 GB | 187 s |
| kubernetes/kubernetes | 161,360 | 578,039 | 48.4 s | 1.11 GB | 58.3 s |

rubygems `log` reports 291 commits with license transitions back to 2004,
including LICENSE.txt moving GPL-1.0-or-later to BSD-2-Clause to MIT.

## Benchmarks

```bash
GITSPDX_BENCH_REPOS=/path/to/repo1:/path/to/repo2 \
  go test -run '^$' -bench BenchmarkScanHistory -benchtime 1x -benchmem
```

Each repository runs once per backend (`catfile`, `gogit`). Reported
metrics: blobs/op, hits/op, MB/op, µs/blob, heap_MiB, plus the standard
ns/op, B/op, allocs/op. The matcher is loaded once and shared across all
subtests; `BenchmarkMatcherLoad` measures that separately.

Add `-cpuprofile` and `-memprofile` to the built binary's `scan` command
for profiling a single run.

## Spike limitations

- `log` walks `--all` refs; shallow clones report every file as added at the
  graft point (a warning is printed).
- Merge diffs are omitted from `log` to avoid counting branch changes
  again. Changes made only while resolving a merge are therefore omitted.
  Legal-path discovery includes merge diffs against the first parent.
- Binary blobs (containing NUL) are skipped.
- `scan` includes unreachable objects in the object store. Legal-path
  eligibility and `log` cover history reachable from refs.
- No `blame` or `diff A..B` subcommands yet.
