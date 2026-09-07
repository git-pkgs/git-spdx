# git-spdx

A Git subcommand for tracing detected licenses across a repository's history.
It shows when license expressions change, when matched files are added or
removed, and when `SPDX-License-Identifier` declarations appear or disappear,
with the commit and path behind each change.
It scans large histories in seconds. That speed makes it practical to trace
license evidence through every revision, even in a large repository.
On an 8-core M1 Pro, the default native Git backend scanned 1.3 GB of blobs
across Cargo's 23,190 commits in an average of 4.05 seconds.

The `licenses` command reports what the current checkout contains. `git-spdx`
adds the timeline, so it can answer questions such as:

- When did a GPL expression first appear in this repository?
- Which commit changed the detected license for a vendored dependency?
- When was an SPDX declaration added to or removed from a source file?
- How has the mix of detected licenses changed month by month?

[git-pkgs/licenses](https://github.com/git-pkgs/licenses) does the license
matching. Its embedded ScanCode rules find license texts, notices, and SPDX
declarations. `git-spdx` adds the history layer: it matches each unique Git
blob once, then compares those results across commits. Repeated content costs
one match even when it appears in many revisions or paths.

## Install

Download a prebuilt archive for macOS, Linux, or Windows from
[GitHub Releases](https://github.com/git-pkgs/git-spdx/releases), then put the
`git-spdx` binary on your `PATH`. The binary name also makes `git spdx`
available.

To build from source with Go 1.26:

```bash
git clone https://github.com/git-pkgs/git-spdx.git
cd git-spdx
go build -o git-spdx .
```

## Quick start

Run these commands inside a repository:

```bash
git spdx log                  # summarize license changes across history
git spdx log --details        # show the commits and paths behind each change
git spdx log --group root     # only root license and notice files
git spdx log --group legal    # only legal files below the repository root
git spdx log --monthly > license-history.csv
```

Pass another local repository as the argument when needed:

```bash
git spdx log /path/to/repository --details
```

For example, a detailed Kubernetes history report finds this change in a
vendored copy of go-yaml:

```text
713b1fc396a1  2018-01-16  bump(gopkg.in/yaml.v2): 670d4cfef0544295bc27a114dbac37980d83185a
  [legal] "vendor/gopkg.in/yaml.v2/LICENSE" (expressions changed)
    - LGPL-3.0-or-later WITH LGPL-3.0-linking-exception
    + Apache-2.0
```

This records a change in the detected evidence. A vendored file, corrected
notice, or newly added declaration can change the report without establishing
that the repository itself was relicensed.

## Commands

### `log`

`git spdx log` reports commits with changes to detected expressions, files, or
SPDX declarations. Add `--details` to print each commit and path.

Paths are split into three groups:

- `root`: recognized legal files in the repository root
- `legal`: recognized legal files in subdirectories, including vendored files
- `other`: every other matched file

Use `--group root`, `--group legal`, or `--group other` to select one group.
Without a filter, the summary includes all three. `--monthly` writes CSV with
commit and change counts by author month and path group.

File additions and deletions are counted separately from expression changes.
The report also tracks files gaining or losing a parsed SPDX declaration, even
when the detected expression stays the same. Repeated notices with the same
expression do not create an expression change.

### `scan`

`git spdx scan` matches every blob in the local object store and prints scan
timings, blob counts, skip reasons, and an expression histogram for the whole
history:

```bash
git spdx scan
git spdx scan /path/to/repository
```

Use `scan` to inspect the complete historical license mix or measure matcher
performance. Use `log` when you need the commits that introduced a change.

## License matching

The `licenses` matcher checks whole-text hashes, parsed
`SPDX-License-Identifier` lines, and license-rule token sequences. Longer
license and notice matches allow copyright holders and years to vary. The
corpus is embedded, and matching runs locally without network access, cgo, or
Python.

Skipped blobs and matcher errors are reported. When one side of a comparison
was skipped, `log` marks it incomplete instead of inferring that a license was
added or removed.

## Backends

The default `git` backend uses parallel `git cat-file` processes and requires
Git on `PATH`. Set `--readers=0` to use `GOMAXPROCS`, which is also the default.

The `gogit` backend reads objects and history in process, so it works without a
Git executable:

```bash
git spdx log --backend=gogit --details
git spdx scan --backend=gogit
```

Run either command with `--help` to see cache, mmap, object reader, and history
worker controls for the Git-free backend.

## Performance

On an 8-core M1 Pro with 16 GB of memory, a full clone of `rust-lang/cargo` at
`a07c49a` contained 23,190 commits and 1.3 GB of historical blobs. Two-run
averages were:

| Backend | Wall time | CPU time | Peak process-tree RSS |
| --- | ---: | ---: | ---: |
| Native Git | 4.05 s | 20.03 s | 2.12 GiB |
| Git-free | 3.57 s | 20.14 s | 614 MiB |

Both backends produced the same normalized output. The benchmark includes all
stored history rather than only the files in the checked-out tree.

To benchmark other repositories:

```bash
GITSPDX_BENCH_REPOS=/path/to/repo1:/path/to/repo2 \
  go test -run '^$' -bench BenchmarkScanHistory -benchtime 1x -benchmem
```

## License

MIT
