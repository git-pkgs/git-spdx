package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

var (
	benchmarkZlibOnce sync.Once
	benchmarkZlibErr  error
)

func TestReadGoGitBlobReusesMemoryObjectContent(t *testing.T) {
	obj := plumbing.NewMemoryObject(nil)
	obj.SetType(plumbing.BlobObject)
	if _, err := obj.Write([]byte("blob content")); err != nil {
		t.Fatal(err)
	}

	got, err := readGoGitBlob(obj)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "blob content" {
		t.Fatalf("content = %q", got)
	}
	if &got[0] != &obj.Bytes()[0] {
		t.Fatal("memory object content was copied")
	}
}

func TestReadGoGitBlobRejectsMemoryObjectSizeMismatch(t *testing.T) {
	obj := plumbing.NewMemoryObject(nil)
	obj.SetType(plumbing.BlobObject)
	if _, err := obj.Write([]byte("blob content")); err != nil {
		t.Fatal(err)
	}
	obj.SetSize(obj.Size() + 1)

	_, err := readGoGitBlob(obj)
	if err == nil || !strings.Contains(err.Error(), "memory object size mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func TestGitFreeTunedPackedScan(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := repository(t, "--object-format="+format)
			for i := range 300 {
				content := fmt.Sprintf("SPDX-License-Identifier: MIT\nunique=%d\n%s", i, strings.Repeat("sample source line\n", 100))
				if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%d.go", i)), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git(t, repo, "add", ".")
			commitFile(t, repo, "LICENSE", "SPDX-License-Identifier: Apache-2.0\n", "Add source files")
			git(t, repo, "repack", "-adq")
			want := cli(t, "scan", repo)
			if metric(t, want, "blobs seen") != 301 {
				t.Fatal(want)
			}
			for _, mode := range []string{"--gogit-memory-index", "--gogit-mmap"} {
				for _, readers := range []string{"1", "2", "4"} {
					args := []string{"scan", repo, "--readers=" + readers, mode, "--gogit-cache-bytes=536870912", "--gogit-cache-shards=8", "--gogit-object-buffer=64", "--gogit-object-batch=64"}
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
	if !strings.Contains(got, "MIT") || !strings.Contains(got, "Apache-2.0") {
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
			got := cliWithoutGit(t, "log", path, "--readers=4", mode, "--gogit-cache-bytes=536870912", "--details")
			if want := cli(t, "log", path, "--details"); got != want {
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

func BenchmarkGoGitObjectInfos(b *testing.B) {
	if os.Getenv("GITSPDX_BENCH_KLAUSPOST") == "1" {
		benchmarkZlibOnce.Do(func() {
			*goGitKlauspostZlib = true
			benchmarkZlibErr = configureGoGitZlib()
		})
		if benchmarkZlibErr != nil {
			b.Fatal(benchmarkZlibErr)
		}
	}
	oldBackend, oldReaders, oldMemory, oldCache, oldMmap, oldObjectInfos := *backend, *readers, *goGitMemoryIndex, *goGitCacheBytes, *goGitMmap, *goGitObjectInfos
	oldBuffer, oldBatch := *goGitObjectBuffer, *goGitObjectBatch
	b.Cleanup(func() {
		*backend, *readers, *goGitMemoryIndex, *goGitCacheBytes = oldBackend, oldReaders, oldMemory, oldCache
		*goGitMmap, *goGitObjectInfos = oldMmap, oldObjectInfos
		*goGitObjectBuffer = oldBuffer
		*goGitObjectBatch = oldBatch
	})
	*backend, *readers, *goGitMemoryIndex, *goGitCacheBytes, *goGitMmap = goGitBackend, 8, false, 96<<20, true
	*goGitObjectBuffer, *goGitObjectBatch = 64, 64
	for _, mode := range []struct {
		name  string
		infos bool
	}{
		{name: "eager", infos: false},
		{name: "metadata", infos: true},
	} {
		*goGitObjectInfos = mode.infos
		for _, root := range benchmarkRepositoryRoots(b) {
			b.Run(mode.name+"/"+filepath.Base(filepath.Clean(root)), func(b *testing.B) {
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
