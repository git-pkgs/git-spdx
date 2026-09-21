package main

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func BenchmarkLegalBlobs(b *testing.B) {
	oldBackend := *backend
	b.Cleanup(func() { *backend = oldBackend })
	*backend = goGitBackend
	for _, root := range benchmarkRepositoryRoots(b) {
		b.Run(filepath.Base(root), func(b *testing.B) {
			b.ReportAllocs()
			var count int
			for b.Loop() {
				legal, err := legalBlobs(root)
				if err != nil {
					b.Fatal(err)
				}
				count = len(legal)
			}
			b.ReportMetric(float64(count), "legal-blobs/op")
		})
	}
}

func BenchmarkLogHistoryPreparation(b *testing.B) {
	oldBackend := *backend
	b.Cleanup(func() { *backend = oldBackend })
	*backend = goGitBackend
	for _, root := range benchmarkRepositoryRoots(b) {
		b.Run(filepath.Base(root)+"/separate", func(b *testing.B) {
			b.ReportAllocs()
			var changes, legalCount int
			for b.Loop() {
				legal, err := legalBlobs(root)
				if err != nil {
					b.Fatal(err)
				}
				legalCount = len(legal)
				changes = 0
				if err := walkChanges(root, false, func(change) { changes++ }); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(changes), "changes/op")
			b.ReportMetric(float64(legalCount), "legal-blobs/op")
		})
		b.Run(filepath.Base(root)+"/single", func(b *testing.B) {
			b.ReportAllocs()
			var changes, legalCount int
			for b.Loop() {
				spool, legal, err := spoolLogHistory(root)
				if err != nil {
					b.Fatal(err)
				}
				legalCount = len(legal)
				changes = 0
				err = spool.replay(func(change) { changes++ })
				spool.close()
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(changes), "changes/op")
			b.ReportMetric(float64(legalCount), "legal-blobs/op")
		})
	}
}

func BenchmarkCommitObjects(b *testing.B) {
	for _, root := range benchmarkRepositoryRoots(b) {
		b.Run(filepath.Base(root), func(b *testing.B) {
			b.ReportAllocs()
			var count int
			for b.Loop() {
				repository, err := openHistory(root)
				if err != nil {
					b.Fatal(err)
				}
				iter, err := repository.Repository().CommitObjects()
				if err != nil {
					b.Fatal(err)
				}
				count = 0
				for {
					_, err := iter.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
					count++
				}
				iter.Close()
				if err := repository.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(count), "commits/op")
		})
	}
}

func BenchmarkReachableCommitTrees(b *testing.B) {
	for _, root := range benchmarkRepositoryRoots(b) {
		b.Run(filepath.Base(root), func(b *testing.B) {
			b.ReportAllocs()
			var count int
			for b.Loop() {
				repository, err := openHistory(root)
				if err != nil {
					b.Fatal(err)
				}
				count = 0
				err = repository.VisitCommitTrees(func(plumbing.Hash) error {
					count++
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				if err := repository.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(count), "commits/op")
		})
	}
}
