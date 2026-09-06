package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testSHA1     = "sha1"
	testSHA256   = "sha256"
	testApache   = "Apache-2.0"
	testMmapFlag = "-gogit-mmap"
)

func TestGitFreeParallelHistory(t *testing.T) {
	for _, format := range []string{testSHA1, testSHA256} {
		t.Run(format, func(t *testing.T) {
			repo := repository(t, "--object-format="+format)
			for n := range 12 {
				date := fmt.Sprintf("2025-01-%02dT12:00:00Z", 13-n)
				t.Setenv("GIT_AUTHOR_DATE", date)
				t.Setenv("GIT_COMMITTER_DATE", date)
				license := []string{"MIT", testApache}[n%2]
				commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: "+license+"\n", fmt.Sprintf("Change %d", n))
			}
			git(t, repo, "gc", "--quiet")
			for _, workers := range []string{"2", "4"} {
				args := []string{"-history-workers=" + workers, testMmapFlag, "-readers=4"}
				for _, mode := range []string{"-details", "-monthly"} {
					got := cliWithoutGit(t, append(args, mode, "log", repo)...)
					want := cli(t, mode, "log", repo)
					if got != want {
						t.Fatalf("history differs:\n%s\n%s", got, want)
					}
				}
			}
		})
	}
}

func TestGitFreeParallelHistoryMissingTree(t *testing.T) {
	repo := repository(t)
	for n := range 12 {
		commitFile(t, repo, "LICENSE", fmt.Sprintf("SPDX-License-Identifier: MIT\n%d\n", n), "Change license")
	}
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD~3^{tree}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(string(out))
	if err := os.Remove(filepath.Join(repo, ".git", "objects", hash[:2], hash[2:])); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd = exec.CommandContext(ctx, os.Args[0], "-backend=gogit", "-history-workers=4", "scan", repo)
	cmd.Env = append(os.Environ(), "GIT_SPDX_TEST_CLI=1", "PATH="+t.TempDir())
	out, err = cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("history workers did not terminate")
	}
	if err == nil || !strings.Contains(string(out), "object not found") {
		t.Fatalf("missing tree accepted: %v\n%s", err, out)
	}
}

func BenchmarkParallelHistory(b *testing.B) {
	oldBackend, oldMmap, oldWorkers := *backend, *goGitMmap, *historyWorkers
	b.Cleanup(func() { *backend, *goGitMmap, *historyWorkers = oldBackend, oldMmap, oldWorkers })
	*backend, *goGitMmap = goGitBackend, true
	for _, root := range benchmarkRepositoryRoots(b) {
		for _, workers := range []int{1, 2, 4} {
			*historyWorkers = workers
			b.Run(fmt.Sprint(workers), func(b *testing.B) {
				b.ReportAllocs()
				var count int
				for b.Loop() {
					count = 0
					if err := walkChanges(root, true, func(change) { count++ }); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(count), "changes/op")
			})
		}
	}
}
