package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/git-pkgs/licenses"
)

var backends = map[string]feeder{
	"catfile": feedBlobsCatFile,
	"gogit":   feedBlobsGoGit,
}

func benchmarkRepositoryRoots(tb testing.TB) []string {
	tb.Helper()
	value := os.Getenv("GITSPDX_BENCH_REPOS")
	if value == "" {
		tb.Skip("set GITSPDX_BENCH_REPOS to a path-list of git repositories")
	}
	var roots []string
	for _, r := range filepath.SplitList(value) {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, r)
		}
	}
	if len(roots) == 0 {
		tb.Skip("GITSPDX_BENCH_REPOS contains no repository paths")
	}
	return roots
}

// BenchmarkScanHistory runs a full-history blob scan on each repository in
// GITSPDX_BENCH_REPOS with each backend. One matcher is shared across all
// subtests. Use -benchtime 1x; a single iteration touches every blob.
func BenchmarkScanHistory(b *testing.B) {
	roots := benchmarkRepositoryRoots(b)
	m, err := licenses.New()
	if err != nil {
		b.Fatal(err)
	}
	for _, root := range roots {
		name := filepath.Base(filepath.Clean(root))
		for backendName, feed := range backends {
			b.Run(name+"/"+backendName, func(b *testing.B) {
				var stats scanStats
				b.ResetTimer()
				for b.Loop() {
					idx := newIndex()
					stats, err = scanBlobsWith(context.Background(), root, m, idx, feed)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(stats.total), "blobs/op")
				b.ReportMetric(float64(stats.hits), "hits/op")
				b.ReportMetric(float64(stats.bytes)/(1<<20), "MB/op")
				if stats.matched > 0 {
					perBlob := float64(stats.elapsed.Nanoseconds()) / float64(stats.matched)
					b.ReportMetric(perBlob/1000, "µs/blob")
				}
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				b.ReportMetric(float64(ms.HeapAlloc)/(1<<20), "heap_MiB")
			})
		}
	}
}

// BenchmarkMatcherLoad measures licenses.New alone so scan results can be
// interpreted without the fixed startup cost.
func BenchmarkMatcherLoad(b *testing.B) {
	for b.Loop() {
		if _, err := licenses.New(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFeedBlobs(b *testing.B) {
	for _, root := range benchmarkRepositoryRoots(b) {
		for name, feed := range backends {
			b.Run(filepath.Base(root)+"/"+name, func(b *testing.B) {
				var total int
				var bytes int64
				b.ReportAllocs()
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
					total, err = feed(root, jobs, func(string) {}, func(string) int64 { return 1 << 20 })
					close(jobs)
					bytes = <-done
					if err != nil {
						b.Fatal(err)
					}
				}
				b.SetBytes(bytes)
				b.ReportMetric(float64(total), "blobs/op")
			})
		}
	}
}

func BenchmarkWalkChanges(b *testing.B) {
	for _, root := range benchmarkRepositoryRoots(b) {
		b.Run(filepath.Base(root), func(b *testing.B) {
			var count int
			b.ReportAllocs()
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
