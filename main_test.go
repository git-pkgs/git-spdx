package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const mitExpression = "MIT"

func TestMain(m *testing.M) {
	if os.Getenv("GIT_SPDX_TEST_CLI") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func metric(t *testing.T, out, name string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, name); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("missing metric %q:\n%s", name, out)
	return 0
}

func TestCLILegalBlobCaps(t *testing.T) {
	repo := repository(t)
	content := "// SPDX-License-Identifier: MIT\n" + strings.Repeat("x\n", 1<<19)
	commitFile(t, repo, "data.txt", content, "Add large ordinary file")
	commitFile(t, repo, "Godeps/LICENSES", content, "Use content as legal bundle")
	git(t, repo, "rm", "Godeps/LICENSES")
	git(t, repo, "commit", "-m", "Remove bundle")
	commitFile(t, repo, "generated.txt", content+"extra\n", "Add unrelated large blob")
	for _, backend := range []string{"git", "gogit"} {
		t.Run(backend, func(t *testing.T) {
			out := cli(t, "scan", repo, "--backend", backend)
			if metric(t, out, "blobs with hits") != 1 || metric(t, out, "skipped: size") != 1 {
				t.Fatalf("legal alias must lift only its own blob cap:\n%s", out)
			}
		})
	}
	out := cli(t, "scan", repo, "--max-legal-blob-size", "1048576")
	if metric(t, out, "skipped: size") != 2 {
		t.Fatalf("explicit legal cap must apply:\n%s", out)
	}
}

func TestListBlobsReportsObjectSizes(t *testing.T) {
	repo := repository(t)
	content := strings.Repeat("large blob contents\n", 100)
	commitFile(t, repo, "large.txt", content, "Add large blob")
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD:large.txt")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(string(out))
	found := false
	_, err = listBlobs(context.Background(), repo, func(blob listedBlob) error {
		if blob.oid == oid {
			found = true
			if blob.size != int64(len(content)) {
				t.Fatalf("blob size = %d, want %d", blob.size, len(content))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("blob %s was not enumerated", oid)
	}
}

func TestCLIIncompleteComparison(t *testing.T) {
	for _, content := range []string{strings.Repeat("x", 2000), "binary\x00content"} {
		t.Run(strconv.Itoa(len(content)), func(t *testing.T) {
			repo := repository(t)
			commitFile(t, repo, "source.go", "// SPDX-License-Identifier: MIT\n", "Add license")
			commitFile(t, repo, "source.go", content, "Unscannable file")
			out := cli(t, "log", repo, "--max-blob-size", "1000", "--details")
			if !strings.Contains(out, "1 incomplete comparisons") || !strings.Contains(out, "0 expression changes") || strings.Contains(out, "    - MIT") {
				t.Fatalf("skipped result must not imply removal:\n%s", out)
			}
		})
	}
}

func TestCLIHistoryGroupsAndPaths(t *testing.T) {
	repo := repository(t)
	for _, name := range []string{"LICENSE", "src/component/LICENSE", "LICENSES/odd\n\t雪.txt", "source.go"} {
		commitFile(t, repo, name, "// SPDX-License-Identifier: MIT\n", "Add "+strconv.Quote(name))
	}
	out := cli(t, "log", repo)
	for _, want := range []string{"root: 1 commits", "legal: 2 commits", "other: 1 commits"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	out = cli(t, "log", repo, "--group", "legal", "--details")
	if !strings.Contains(out, `"LICENSES/odd\n\t雪.txt"`) || strings.Contains(out, "[other]") || strings.Contains(out, "root:") {
		t.Fatalf("filter or NUL-delimited path handling failed:\n%s", out)
	}
}

func TestCLILogUsesSingleHistoryWalk(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
	commitFile(t, repo, "source.go", "// SPDX-License-Identifier: MIT\n", "Add source")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GIT_SPDX_GIT_TRACE\"\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "git.log")
	cmd := exec.Command(os.Args[0], "log", repo)
	cmd.Env = append(os.Environ(),
		"GIT_SPDX_TEST_CLI=1",
		"GIT_SPDX_GIT_TRACE="+trace,
		"GOMAXPROCS=2",
		"PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git spdx log: %v\n%s", err, out)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	walks := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(" "+line+" ", " log ") {
			walks++
		}
	}
	if walks != 1 {
		t.Fatalf("git log subprocesses = %d, want 1\n%s", walks, data)
	}
}

const mitNotice = `Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
`

func TestCLISPDXDeclarationWithoutExpressionChange(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "source.c", mitNotice, "Add notice")
	commitFile(t, repo, "source.c", mitNotice+mitNotice, "Repeat notice")
	commitFile(t, repo, "source.c", "// SPDX-License-Identifier: MIT\n"+mitNotice, "Add explicit declaration")
	out := cli(t, "log", repo, "--details")
	if strings.Contains(out, "Repeat notice") || !strings.Contains(out, "Add explicit declaration") || !strings.Contains(out, "0 expression changes") || !strings.Contains(out, "1 SPDX declarations added") {
		t.Fatalf("declarations and repeated notices must be separate from expression changes:\n%s", out)
	}
}

func TestCLICorpusCoveredSPDXDeclaration(t *testing.T) {
	repo := repository(t)
	source := "int example(void) { return 0; }\n"
	commitFile(t, repo, "source.c", source, "Add source")
	commitFile(t, repo, "source.c", "// SPDX-License-Identifier: GPL-2.0\n"+source, "Add GPL declaration")
	out := cli(t, "log", repo, "--details")
	if !strings.Contains(out, "1 SPDX declarations added") || !strings.Contains(out, "SPDX declaration: false -> true") {
		t.Fatalf("a corpus-covered tag must count as a declaration:\n%s", out)
	}
}

func TestCLISHA256(t *testing.T) {
	repo := repository(t, "--object-format=sha256")
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Change license")
	git(t, repo, "rm", "LICENSE")
	git(t, repo, "commit", "-m", "Remove license file")
	out := cli(t, "log", repo)
	if !strings.Contains(out, "root: 3 commits; 1 expression changes; 1 file additions; 1 file deletions;") {
		t.Fatalf("SHA-256 history failed:\n%s", out)
	}
}

func TestCLIMonthlyCounts(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "source.c", mitNotice, "Add notice")
	commitFile(t, repo, "source.c", "// SPDX-License-Identifier: MIT\n"+mitNotice, "Add declaration")
	out := cli(t, "log", repo, "--monthly")
	if !strings.HasPrefix(out, "month,group,commits,expression_changes,file_additions,file_deletions,incomplete,spdx_added,spdx_removed\n") || !strings.Contains(out, ",other,2,0,1,0,0,1,0\n") {
		t.Fatalf("monthly CSV must distinguish declaration additions:\n%s", out)
	}
}

func TestIndexSHA256SuffixAndSkippedState(t *testing.T) {
	idx := newIndex()
	first := strings.Repeat("a", 40) + strings.Repeat("0", 24)
	second := strings.Repeat("a", 40) + strings.Repeat("1", 24)
	idx.put(first, blobResult{expressions: []string{mitExpression}})
	idx.put(second, blobResult{skipped: skippedError})
	if got := lookup(idx, first); !equalExprs(got.expressions, []string{mitExpression}) || got.skipped != notSkipped {
		t.Fatalf("first SHA-256 result changed: %+v", got)
	}
	if got := lookup(idx, second); got.skipped != skippedError {
		t.Fatalf("second SHA-256 result lost its error: %+v", got)
	}
}

func TestIndexKeepsDeclarationWithoutDetection(t *testing.T) {
	idx := newIndex()
	oid := strings.Repeat("a", 40)
	idx.put(oid, blobResult{spdx: true})
	if got := lookup(idx, oid); !got.spdx {
		t.Fatalf("declaration-only result was discarded: %+v", got)
	}
}

func TestCLIEmptyRepository(t *testing.T) {
	repo := repository(t)
	out := cli(t, "scan", repo)
	if metric(t, out, "blobs seen") != 0 {
		t.Fatalf("empty scan:\n%s", out)
	}
	out = cli(t, "log", repo)
	if !strings.Contains(out, "root: 0 commits;") {
		t.Fatalf("empty history:\n%s", out)
	}
}

func TestCLIHelp(t *testing.T) {
	out := cli(t, "log", "--help")
	for _, option := range []string{"-max-legal-blob-size", "-group", "-monthly"} {
		if !strings.Contains(out, option) {
			t.Fatalf("help is missing %s:\n%s", option, out)
		}
	}
	if strings.Contains(out, "--cpuprofile") {
		t.Fatalf("log help includes scan-only flags:\n%s", out)
	}
	scanHelp := cli(t, "scan", "--help")
	if !strings.Contains(scanHelp, "--cpuprofile") || strings.Contains(scanHelp, "--monthly") {
		t.Fatalf("scan help has the wrong flags:\n%s", scanHelp)
	}
}

func TestCLIFlagsMaySurroundRepository(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
	before := cli(t, "log", "--details", repo)
	after := cli(t, "log", repo, "--details")
	if before != after {
		t.Fatalf("flag placement changed output:\nbefore repository:\n%s\nafter repository:\n%s", before, after)
	}
}

func TestCLIExpressionSummaryBreaksCountTiesByName(t *testing.T) {
	repo := repository(t)
	ids := []string{
		"0BSD", "AGPL-3.0-only", "Apache-2.0", "Artistic-2.0", "BlueOak-1.0.0",
		"BSD-2-Clause", "BSD-3-Clause", "BSL-1.0", "CC0-1.0", "CDDL-1.0",
		"EPL-2.0", "EUPL-1.2", "GPL-2.0-only", "GPL-3.0-only", "ISC",
		"LGPL-2.1-only", "MIT", "MIT-0", "MPL-2.0", "Unlicense", "WTFPL", "Zlib",
	}
	for index, id := range ids {
		name := filepath.Join(repo, "file-"+strconv.Itoa(index)+".txt")
		if err := os.WriteFile(name, []byte("SPDX-License-Identifier: "+id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "Add SPDX declarations")

	out := cli(t, "scan", repo)
	summary := strings.Split(out, "expressions across all history:\n")
	if len(summary) != 2 {
		t.Fatal(out)
	}
	var got []string
	for _, line := range strings.Split(summary[1], "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "1" {
			continue
		}
		got = append(got, fields[1])
	}
	if len(got) != expressionDisplayLimit || !slices.IsSorted(got) {
		t.Fatalf("expression summary is not sorted by name: %q\n%s", got, out)
	}
}

func TestCLILegalPathIntroducedByMerge(t *testing.T) {
	repo := repository(t)
	content := "SPDX-License-Identifier: MIT\n" + strings.Repeat("x\n", 100)
	commitFile(t, repo, "data.txt", content, "Add ordinary blob")
	git(t, repo, "switch", "-c", "side")
	commitFile(t, repo, "side.txt", "side\n", "Side change")
	git(t, repo, "switch", "main")
	commitFile(t, repo, "main.txt", "main\n", "Main change")
	git(t, repo, "merge", "--no-commit", "--no-ff", "side")
	commitFile(t, repo, "Godeps/LICENSES", content, "Add legal path during merge")
	git(t, repo, "rm", "Godeps/LICENSES")
	git(t, repo, "commit", "-m", "Remove legal path")
	out := cli(t, "scan", repo, "--max-blob-size", "64", "--max-legal-blob-size", "1024")
	if metric(t, out, "blobs with hits") != 1 || metric(t, out, "skipped: size") != 0 {
		t.Fatalf("a merge-only legal path must contribute to blob eligibility:\n%s", out)
	}
}

func TestCLIMatcherErrorRemainsIncomplete(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSES/generated.txt", "SPDX-License-Identifier: MIT\n", "Add license")
	commitFile(t, repo, "LICENSES/generated.txt", strings.Repeat("mit license ", 400_000), "Exceed matcher candidate limit")
	out := cli(t, "scan", repo)
	if metric(t, out, "skipped: error") != 1 {
		t.Fatalf("matcher errors must be counted:\n%s", out)
	}
	out = cli(t, "log", repo, "--details")
	if !strings.Contains(out, "before=scanned, after=error") || !strings.Contains(out, "1 incomplete comparisons") || strings.Contains(out, "    - MIT") {
		t.Fatalf("matcher errors must not imply license removal:\n%s", out)
	}
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repository(t *testing.T, options ...string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, append([]string{"init", "-b", "main"}, options...)...)
	git(t, repo, "config", "user.name", "Test")
	git(t, repo, "config", "user.email", "test@example.org")
	git(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

func commitFile(t *testing.T, repo, name, content, subject string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "--", name)
	git(t, repo, "commit", "-m", subject)
}

func cli(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GIT_SPDX_TEST_CLI=1", "GOMAXPROCS=2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git spdx %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestCLI(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "source.go", "// SPDX-License-Identifier: MIT\npackage example\n", "Add source")
	commitFile(t, repo, "source.go", "// SPDX-License-Identifier: Apache-2.0\npackage example\n", "Change expression")
	for _, command := range []string{"scan", "log"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, repo, "--readers", "2"}
			if command == "log" {
				args = append(args, "--details")
			}
			out := cli(t, args...)
			for _, expression := range []string{mitExpression, "Apache-2.0"} {
				if !strings.Contains(out, expression) {
					t.Fatalf("missing %s:\n%s", expression, out)
				}
			}
		})
	}
}

func TestCLIBenchmarkPhases(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
	cmd := exec.Command(os.Args[0], "scan", repo, "--backend=gogit")
	cmd.Env = append(os.Environ(), "GIT_SPDX_TEST_CLI=1", "GITSPDX_BENCH_PHASES=1", "GOMAXPROCS=2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git spdx: %v\n%s", err, out)
	}
	for _, phase := range []string{"legal", "feed", "match"} {
		if !strings.Contains(string(out), "bench-phase name="+phase+" ") {
			t.Fatalf("missing %s phase:\n%s", phase, out)
		}
	}
}
