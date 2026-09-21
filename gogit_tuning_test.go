package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultGoGitTuning(t *testing.T) {
	got := goGitTuning()
	if !got.Mmap || !got.KlauspostZlib {
		t.Fatalf("default tuning = %+v", got)
	}
}

func TestDefaultGoGitReaders(t *testing.T) {
	oldReaders := *readers
	t.Cleanup(func() { *readers = oldReaders })
	*readers = 0
	if got, want := goGitReaders(), runtime.GOMAXPROCS(0); got != want {
		t.Fatalf("readers = %d, want %d", got, want)
	}
}

func TestDefaultHistoryWorkers(t *testing.T) {
	if got, want := *historyWorkers, min(defaultHistoryWorkers, runtime.GOMAXPROCS(0)); got != want {
		t.Fatalf("history workers = %d, want %d", got, want)
	}
}

func TestGitFreeTunedPackedScan(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := repository(t, "--object-format="+format)
			const sourceFiles = 1100
			for i := range sourceFiles {
				content := fmt.Sprintf("SPDX-License-Identifier: MIT\nunique=%d\n%s", i, strings.Repeat("sample source line\n", 100))
				if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%d.go", i)), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git(t, repo, "add", ".")
			commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Add source files")
			git(t, repo, "repack", "-adq")
			want := cli(t, testScanCommand, repo)
			if metric(t, want, "blobs seen") != sourceFiles+1 {
				t.Fatal(want)
			}
			for _, mode := range []string{"--gogit-memory-index", "--gogit-mmap"} {
				for _, readers := range []string{"1", "2", "4"} {
					args := []string{testScanCommand, repo, "--readers=" + readers, mode, "--gogit-cache-bytes=536870912", "--gogit-cache-shards=8"}
					got := cliWithoutGit(t, args...)
					assertScanMetrics(t, got, want)
				}
			}
		})
	}
}

func assertScanMetrics(t *testing.T, got, want string) {
	t.Helper()
	for _, name := range []string{"blobs seen", "blobs matched", "blobs with hits", "skipped: size", "skipped: binary", "skipped: error"} {
		if metric(t, got, name) != metric(t, want, name) {
			t.Fatalf("%s differs:\n%s\n%s", name, got, want)
		}
	}
	if !strings.Contains(got, mitExpression) || !strings.Contains(got, testApache) {
		t.Fatal(got)
	}
}

func TestGitFreeTunedHistoryLayouts(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: MIT\n", "Add license")
	commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Change license")
	git(t, repo, "gc", "--quiet")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, repo, "clone", "--bare", "--no-local", repo, bare)
	worktree := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	shallow := filepath.Join(t.TempDir(), "shallow")
	git(t, repo, "clone", "--depth=1", "file://"+repo, shallow)
	for _, path := range []string{repo, bare, worktree, shallow} {
		for _, mode := range []string{"--gogit-memory-index", "--gogit-mmap"} {
			got := cliWithoutGit(t, testLogCommand, path, "--readers=4", mode, "--gogit-cache-bytes=536870912", "--details")
			if want := cli(t, testLogCommand, path, "--details"); got != want {
				t.Fatalf("history differs for %s:\n%s\n%s", path, got, want)
			}
		}
	}
}

func BenchmarkGoGitTuning(b *testing.B) {
	oldBackend, oldReaders, oldMemory, oldCache, oldMmap := *backend, *readers, *goGitMemoryIndex, *goGitCacheBytes, *goGitMmap
	b.Cleanup(func() {
		*backend, *readers, *goGitMemoryIndex, *goGitCacheBytes = oldBackend, oldReaders, oldMemory, oldCache
		*goGitMmap = oldMmap
	})
	for _, root := range benchmarkRepositoryRoots(b) {
		for _, config := range []struct {
			name   string
			memory bool
			cache  uint64
			mmap   bool
		}{
			{"default", false, 96 << 20, false},
			{"memory", true, 96 << 20, false},
			{"mmap", false, 96 << 20, true},
		} {
			*backend, *goGitMemoryIndex, *goGitCacheBytes = goGitBackend, config.memory, config.cache
			*goGitMmap = config.mmap
			b.Run(config.name+"/history", func(b *testing.B) {
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
			for _, workers := range []int{1, 2, 4, 8, 10, 12, 16} {
				*readers = workers
				b.Run(fmt.Sprintf("%s/readers%d", config.name, workers), func(b *testing.B) {
					benchmarkTunedFeed(b, root)
				})
			}
		}
	}
}

func BenchmarkGoGitCache(b *testing.B) {
	oldBackend, oldReaders, oldMemory, oldCache, oldMmap := *backend, *readers, *goGitMemoryIndex, *goGitCacheBytes, *goGitMmap
	b.Cleanup(func() {
		*backend, *readers, *goGitMemoryIndex, *goGitCacheBytes = oldBackend, oldReaders, oldMemory, oldCache
		*goGitMmap = oldMmap
	})
	*backend, *readers, *goGitMemoryIndex, *goGitMmap = goGitBackend, 1, false, true
	for _, root := range benchmarkRepositoryRoots(b) {
		for _, mib := range []uint64{96, 256, 512, 576, 640, 768, 1024} {
			*goGitCacheBytes = mib << 20
			b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
				benchmarkTunedFeed(b, root)
			})
		}
	}
}

func BenchmarkGoGitCacheReaders4(b *testing.B) {
	oldBackend, oldReaders, oldMemory, oldCache, oldMmap := *backend, *readers, *goGitMemoryIndex, *goGitCacheBytes, *goGitMmap
	b.Cleanup(func() {
		*backend, *readers, *goGitMemoryIndex, *goGitCacheBytes = oldBackend, oldReaders, oldMemory, oldCache
		*goGitMmap = oldMmap
	})
	*backend, *readers, *goGitMemoryIndex, *goGitMmap = goGitBackend, 4, false, true
	for _, root := range benchmarkRepositoryRoots(b) {
		for _, mib := range []uint64{96, 256, 576} {
			*goGitCacheBytes = mib << 20
			b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
				benchmarkTunedFeed(b, root)
			})
		}
	}
}

func benchmarkTunedFeed(b *testing.B, root string) {
	b.Helper()
	b.ReportAllocs()
	var total int
	var bytes int64
	for b.Loop() {
		jobs := make(chan job, jobQueueSize)
		done := make(chan int64, 1)
		go func() {
			var n int64
			for j := range jobs {
				n += int64(len(j.data))
			}
			done <- n
		}()
		var err error
		total, err = feedBlobsGoGit(root, jobs, func(string) {}, func(string) int64 { return 1 << 20 })
		close(jobs)
		bytes = <-done
		if err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(bytes)
	b.ReportMetric(float64(total), "blobs/op")
}
