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
				repository, err := openGoGit(root)
				if err != nil {
					b.Fatal(err)
				}
				iter, err := repository.CommitObjects()
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
				repository, err := openGoGit(root)
				if err != nil {
					b.Fatal(err)
				}
				roots, err := historyRoots(repository)
				if err != nil {
					b.Fatal(err)
				}
				shallow, err := repository.Storer.Shallow()
				if err != nil {
					b.Fatal(err)
				}
				boundary := make(map[plumbing.Hash]bool, len(shallow))
				for _, hash := range shallow {
					boundary[hash] = true
				}
				seen := make(map[plumbing.Hash]struct{})
				stack := append([]plumbing.Hash(nil), roots...)
				for len(stack) > 0 {
					last := len(stack) - 1
					hash := stack[last]
					stack = stack[:last]
					if _, ok := seen[hash]; ok {
						continue
					}
					commit, err := repository.CommitObject(hash)
					if err != nil {
						b.Fatal(err)
					}
					seen[hash] = struct{}{}
					if !boundary[hash] {
						stack = append(stack, commit.ParentHashes...)
					}
				}
				count = len(seen)
				if err := repository.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(count), "commits/op")
		})
	}
}
