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
git spdx log  [repo]   # print commits where a path's detected expressions changed
```

Drop the `git-spdx` binary on `$PATH` to use it as a git subcommand.

## Numbers

M1 Pro, 8 cores, 16 GB, go1.26.7.

| repo | commits | blobs | scan wall | peak RSS | log wall |
|---|---|---|---|---|---|
| rust-lang/cargo | 23,052 | 57,529 | 6.1 s | 306 MB | - |
| rubygems/rubygems | 48,222 | 91,478 | 4.4 s | 476 MB | 8.1 s |
| homebrew-core | 828,252 | 708,799 | 17.6 s | 1.30 GB | 187 s |

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
- Merge commits use git's default raw diff; `--diff-merges` handling is not
  configured yet.
- Blobs over 1 MiB are skipped. Binary blobs (containing NUL) are skipped.
- No `blame` or `diff A..B` subcommands yet.
- `git log --raw` output is buffered whole; large repos should stream it.
