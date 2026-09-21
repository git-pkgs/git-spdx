package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cliWithoutGit(t *testing.T, args ...string) string {
	t.Helper()
	commandArgs := append([]string{}, args...)
	commandArgs = append(commandArgs, "--backend=gogit", "--history-workers=4")
	cmd := exec.Command(os.Args[0], commandArgs...)
	cmd.Env = append(os.Environ(), "GIT_SPDX_TEST_CLI=1", "GOMAXPROCS=2", "PATH="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Git-free CLI %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestGitFreePackedHistory(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := repository(t, "--object-format="+format)
			commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
			commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Change license")
			commitFile(t, repo, testLegalPath(), "SPDX-License-Identifier: MIT\n", "Add vendor notice")
			git(t, repo, "gc", "--quiet")
			want := cli(t, testLogCommand, repo, "--details")
			got := cliWithoutGit(t, testLogCommand, repo, "--details")
			if got != want {
				t.Fatalf("history differs:\nwant:\n%s\ngot:\n%s", want, got)
			}
			out := cliWithoutGit(t, testScanCommand, repo)
			if metric(t, out, "blobs with hits") != 2 {
				t.Fatal(out)
			}
		})
	}
}

func TestGitFreeMergeLegalPathAndTag(t *testing.T) {
	repo := repository(t)
	content := "SPDX-License-Identifier: MIT\n" + strings.Repeat("x\n", 100)
	commitFile(t, repo, "data.txt", content, "Add ordinary blob")
	git(t, repo, "switch", "-c", "side")
	commitFile(t, repo, "side.txt", "side\n", "Side change")
	git(t, repo, "switch", "main")
	commitFile(t, repo, "main.txt", "main\n", "Main change")
	git(t, repo, "merge", "--no-commit", "--no-ff", "side")
	commitFile(t, repo, "Godeps/LICENSES", content, "Legal alias in merge")
	git(t, repo, "rm", "Godeps/LICENSES")
	git(t, repo, "commit", "-m", "Remove alias")
	git(t, repo, "switch", "--orphan", "tagged")
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Tag-only license")
	git(t, repo, "tag", "-a", "tag-only", "-m", "Tagged orphan")
	git(t, repo, "switch", "main")
	git(t, repo, "branch", "-D", "tagged")
	args := []string{testScanCommand, repo, "--max-blob-size=64", "--max-legal-blob-size=1024"}
	out := cliWithoutGit(t, args...)
	if metric(t, out, "blobs with hits") != 2 || metric(t, out, "skipped: size") != 0 {
		t.Fatal(out)
	}
	args = []string{testLogCommand, repo, "--max-blob-size=64", "--max-legal-blob-size=1024", "--monthly"}
	if got, want := cliWithoutGit(t, args...), cli(t, args...); got != want {
		t.Fatalf("monthly counts differ:\n%s\n%s", got, want)
	}
}

func TestGitFreeSharedSubtreeUsesRolesState(t *testing.T) {
	repo := repository(t)
	content := "SPDX-License-Identifier: MIT\n" + strings.Repeat("x\n", 100)
	commitFile(t, repo, "docs/component.txt", content, "Add ordinary copy")
	commitFile(t, repo, "licenses/component.txt", content, "Add legal copy")

	var tree string
	for _, directory := range []string{"docs", "licenses"} {
		cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD:"+directory)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		current := strings.TrimSpace(string(out))
		if tree != "" && current != tree {
			t.Fatalf("tree for %s = %s, want shared tree %s", directory, current, tree)
		}
		tree = current
	}

	out := cliWithoutGit(t, testScanCommand, repo, "--max-blob-size=64", "--max-legal-blob-size=1024")
	if metric(t, out, "blobs with hits") != 1 || metric(t, out, "skipped: size") != 0 {
		t.Fatalf("shared subtree lost its legal occurrence:\n%s", out)
	}
}

func TestGitFreeBareWorktreeAndShallow(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Change license")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, repo, "clone", "--bare", "--no-local", repo, bare)
	worktree := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	shallow := filepath.Join(t.TempDir(), "shallow")
	git(t, repo, "clone", "--depth=1", "file://"+repo, shallow)
	for _, path := range []string{bare, worktree, shallow} {
		want := cli(t, testLogCommand, path, "--details")
		got := cliWithoutGit(t, testLogCommand, path, "--details")
		if got != want {
			t.Fatalf("history for %s differs:\nwant:\n%s\ngot:\n%s", path, want, got)
		}
	}
}

func TestGitFreeEmptyRepository(t *testing.T) {
	repo := repository(t)
	if out := cliWithoutGit(t, testScanCommand, repo); metric(t, out, "blobs seen") != 0 {
		t.Fatal(out)
	}
	if out := cliWithoutGit(t, testLogCommand, repo); !strings.Contains(out, "root: 0 commits") {
		t.Fatal(out)
	}
}

func TestGitFreeAlternateObjectStore(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Change license")
	git(t, repo, "gc", "--quiet")
	shared := filepath.Join(t.TempDir(), "shared")
	git(t, repo, "clone", "--shared", repo, shared)
	want := cliWithoutGit(t, testScanCommand, repo)
	got := cliWithoutGit(t, testScanCommand, shared)
	for _, name := range []string{
		"blobs seen", "blobs matched", "blobs with hits",
		"skipped: size", "skipped: binary", "skipped: error",
	} {
		if gotMetric, wantMetric := metric(t, got, name), metric(t, want, name); gotMetric != wantMetric {
			t.Fatalf("%s=%d, want %d\nsource:\n%s\nshared:\n%s", name, gotMetric, wantMetric, want, got)
		}
	}
}

func TestGitFreeHistoryWithClockSkew(t *testing.T) {
	repo := repository(t)
	for i, date := range []string{"2030-01-01T12:00:00Z", "2020-01-01T12:00:00Z", "2025-01-01T12:00:00Z"} {
		t.Setenv("GIT_AUTHOR_DATE", date)
		t.Setenv("GIT_COMMITTER_DATE", date)
		content := []string{mitExpression, testApache, "BSD-3-Clause"}[i]
		commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: "+content+"\n", "License "+content)
	}
	if got, want := cliWithoutGit(t, testLogCommand, repo, "--details"), cli(t, testLogCommand, repo, "--details"); got != want {
		t.Fatalf("clock-skewed history differs:\n%s\n%s", got, want)
	}
}

func TestGitFreeScanRejectsCorruptBlobChecksum(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSE", strings.Repeat("SPDX-License-Identifier: MIT\n", 100), "Add license")
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD:LICENSE")
	oid, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", repo, "pack-objects", ".git/objects/pack/pack")
	cmd.Stdin = strings.NewReader(string(oid))
	packHash, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	git(t, repo, "prune-packed")
	path := filepath.Join(repo, ".git", "objects", "pack", "pack-"+strings.TrimSpace(string(packHash))+".pack")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-21] ^= 1
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, klauspost := range []bool{false, true} {
		cmd = exec.Command(os.Args[0], testScanCommand, repo, "--backend=gogit", "--readers=4", "--gogit-memory-index", fmt.Sprintf("--gogit-klauspost-zlib=%t", klauspost))
		cmd.Env = append(os.Environ(), "GIT_SPDX_TEST_CLI=1", "GOMAXPROCS=2", "PATH="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "checksum") {
			t.Fatalf("corrupt payload must fail with klauspost=%t: %v\n%s", klauspost, err, out)
		}
	}
}
