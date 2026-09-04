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
					idx := &index{blobs: make(map[string]blobResult, 1<<16)}
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
