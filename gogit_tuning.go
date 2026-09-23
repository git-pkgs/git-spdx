package main

import (
	"runtime"

	githistory "github.com/git-pkgs/history"
)

const defaultHistoryWorkers = 4

var defaultGoGitTuning = githistory.DefaultTuning()

var (
	goGitMemoryIndex   = newOption(defaultGoGitTuning.MemoryIndex)
	goGitMmap          = newOption(defaultGoGitTuning.Mmap)
	goGitCacheBytes    = newOption(defaultGoGitTuning.CacheBytes)
	goGitCacheShards   = newOption(defaultGoGitTuning.CacheShards)
	goGitKlauspostZlib = newOption(defaultGoGitTuning.KlauspostZlib)
	historyWorkers     = newOption(min(defaultHistoryWorkers, runtime.GOMAXPROCS(0)))
)

func goGitTuning() githistory.Tuning {
	return githistory.Tuning{
		MemoryIndex:   *goGitMemoryIndex,
		Mmap:          *goGitMmap,
		CacheBytes:    *goGitCacheBytes,
		CacheShards:   *goGitCacheShards,
		KlauspostZlib: *goGitKlauspostZlib,
	}
}

func openHistory(path string) (*githistory.Repo, error) {
	return githistory.OpenWithOptions(path, githistory.OpenOptions{Tuning: goGitTuning()})
}

func goGitReaders() int {
	if *readers > 0 {
		return *readers
	}
	return runtime.GOMAXPROCS(0)
}

func feedBlobsGoGit(repo string, jobs chan<- job, skip func(string), limit func(string) int64) (int, error) {
	r, err := openHistory(repo)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	return r.WalkBlobs(githistory.BlobOptions{
		Workers: goGitReaders(),
		Limit:   limit,
		Skip:    skip,
	}, func(blob githistory.Blob) error {
		jobs <- job{oid: blob.OID, data: blob.Data}
		return nil
	})
}
