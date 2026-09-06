# git-spdx

Find when detected licenses and SPDX declarations changed across a Git
repository's entire history. git-spdx matches each unique blob once and
replays commit history against the cached results, avoiding a full scan of
every revision.

The default backend uses native Git subprocesses. The `gogit` backend reads
the repository and its history in Go, so the binary can run on a machine
without Git installed. License detection comes from
[git-pkgs/licenses](https://github.com/git-pkgs/licenses).

```bash
go build -o git-spdx .
```

The module pins public fork patches through `replace` directives, so
`go install github.com/git-pkgs/git-spdx@latest` refuses to install it.
Clone the repository and run the build command above until those changes land
upstream. [git-pkgs/go-git](https://github.com/git-pkgs/go-git) carries the
correctness and packed-object reader changes prepared for upstream review;
[git-pkgs/go-billy](https://github.com/git-pkgs/go-billy) supplies its mmap
file views.

## Commands

```bash
git spdx scan [repo]   # match every blob, print throughput and expression histogram
git spdx log  [repo]   # summarise detected changes by path group
git spdx -details log [repo]  # include individual commits and paths
git spdx -group legal -details log [repo]
git spdx -monthly log [repo] > monthly.csv
git spdx -backend=gogit scan [repo]  # run without Git subprocesses
```

Drop the `git-spdx` binary on `$PATH` to use it as a git subcommand.
Options go before the command.

`log` groups paths as `root` (root legal files), `legal` (legal files in
subdirectories), and `other`. Legal filenames and directories use
`licenses.LegalFileRoles`; a subdirectory does not establish that a file
is vendored. Use `-group root`, `-group legal`, or `-group other` to filter
the report.

## Example

Kubernetes picked up go-yaml's LGPL-to-Apache license change on 2018-01-16,
18 months after the upstream change. `git spdx -details -group legal log`
reported the vendored transition:

```text
713b1fc396a1  2018-01-16  bump(gopkg.in/yaml.v2): 670d4cf...
  [legal] "vendor/gopkg.in/yaml.v2/LICENSE" (expressions changed)
    - LGPL-3.0-or-later WITH LGPL-3.0-linking-exception
    + Apache-2.0
```

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

## Backends

Native Git remains the default. It uses parallel `git cat-file` processes and
accepts `-readers=0` as `GOMAXPROCS`. Peak memory comparisons should include
the `git-spdx` process and all of its child processes.

The Git-free backend supports pack index mmap, parallel object readers,
parallel history work, a sharded object cache, and klauspost zlib. The settings
used for the Cargo benchmark below were:

```bash
git spdx -backend=gogit -gogit-mmap -gogit-klauspost-zlib \
  -readers=8 -history-workers=6 \
  -gogit-cache-bytes=100663296 -gogit-cache-shards=8 scan [repo]
```

Repositories using replacement refs, grafts, object-directory environment
overrides, or non-empty object alternates return an error under the Git-free
backend. This prevents partial history scans.

## Numbers

Two-run averages on an M1 Pro with 8 cores and 16 GB, scanning a full clone of
`rust-lang/cargo` with 23,190 commits dated 2014-03-04 through 2026-09-05 and
189,074 packed objects:

| backend | wall | CPU | peak process-tree RSS |
|---|---:|---:|---:|
| native Git | 4.05 s | 20.03 s | 2.12 GiB |
| Git-free | 3.57 s | 20.14 s | 614 MiB |

Both backends reported 58,000 blobs seen, 57,998 matched, 5,765 with license
hits, two size skips, and 33 binary skips. Their normalized output was equal.
The checked-out Cargo tree at `a07c49a` has 2,950 files and takes 0.65 s to
scan with `licenses` using its default flags. The git-spdx timing covers the
1.3 GB of blobs stored across the repository rather than only that tree.

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

## Limitations

- `log` walks `--all` refs; shallow clones report every file as added at the
  graft point (a warning is printed).
- Merge diffs are omitted from `log` to avoid counting branch changes
  again. Changes made only while resolving a merge are therefore omitted.
  Legal-path discovery includes merge diffs against the first parent.
- Binary blobs (containing NUL) are skipped.
- Symlink blobs are scanned as text, so a target path that resembles license
  text can produce a match.
- `scan` includes unreachable objects in the object store. Legal-path
  eligibility and `log` cover history reachable from refs.
- Legal-path caps follow `licenses.LegalFileRoles`. Names such as
  `license.rs` are included; `THIRD_PARTY_LICENSES.txt` and `PATENTS` are not.
- No `blame` or `diff A..B` subcommands yet.
