package main

import (
	"context"
	"fmt"
	"sync"

	githistory "github.com/git-pkgs/history"
	"github.com/git-pkgs/roles"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

type legalTreeKey struct {
	hash  plumbing.Hash
	state roles.StateKey
}

type legalTreeCollector struct {
	mu    sync.Mutex
	seen  map[legalTreeKey]struct{}
	legal map[string]bool
}

const (
	legalTreeBatchSize        = 48
	legalTreeBatchesPerWorker = 2
	gitFileTypeMask           = 0o170000
)

type legalTreeBatch struct {
	hashes [legalTreeBatchSize]plumbing.Hash
	count  int
}

func newLegalTreeCollector() *legalTreeCollector {
	return &legalTreeCollector{
		seen:  make(map[legalTreeKey]struct{}),
		legal: make(map[string]bool),
	}
}

func (c *legalTreeCollector) claim(key legalTreeKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		return false
	}
	c.seen[key] = struct{}{}
	return true
}

func (c *legalTreeCollector) add(hash plumbing.Hash) {
	c.mu.Lock()
	c.legal[hash.String()] = true
	c.mu.Unlock()
}

func legalBlobsGoGit(path string) (map[string]bool, error) {
	repo, err := openHistory(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = repo.Close() }()
	collector := newLegalTreeCollector()
	if *historyWorkers <= 1 {
		err = repo.VisitCommitTrees(func(hash plumbing.Hash) error {
			return collectLegalTreeRaw(repo, hash, roles.RootState(), collector)
		})
		return collector.legal, err
	}
	err = collectLegalTreesParallel(repo, collector, *historyWorkers-1)
	return collector.legal, err
}

func collectLegalTreesParallel(repo *githistory.Repo, collector *legalTreeCollector, workers int) error {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	tasks := make(chan legalTreeBatch, workers*legalTreeBatchesPerWorker)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { collectLegalTreeBatches(ctx, cancel, repo, tasks, collector) })
	}
	var batch legalTreeBatch
	enqueue := func() error {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case tasks <- batch:
			batch.count = 0
			return nil
		}
	}
	err := repo.VisitCommitTrees(func(hash plumbing.Hash) error {
		batch.hashes[batch.count] = hash
		batch.count++
		if batch.count == len(batch.hashes) {
			return enqueue()
		}
		return nil
	})
	if err == nil && batch.count > 0 {
		err = enqueue()
	}
	close(tasks)
	wg.Wait()
	if err != nil {
		return err
	}
	return context.Cause(ctx)
}

func collectLegalTreeBatches(ctx context.Context, fail context.CancelCauseFunc, r *githistory.Repo, tasks <-chan legalTreeBatch, collector *legalTreeCollector) {
	for {
		select {
		case <-ctx.Done():
			return
		case batch, ok := <-tasks:
			if !ok {
				return
			}
			if err := collectLegalTreeBatch(r, batch, collector); err != nil {
				fail(err)
				return
			}
		}
	}
}

func collectLegalTreeBatch(r *githistory.Repo, batch legalTreeBatch, collector *legalTreeCollector) error {
	for _, hash := range batch.hashes[:batch.count] {
		if err := collectLegalTreeRaw(r, hash, roles.RootState(), collector); err != nil {
			return err
		}
	}
	return nil
}

func collectLegalTreeRaw(r *githistory.Repo, hash plumbing.Hash, state roles.State, collector *legalTreeCollector) error {
	key := legalTreeKey{hash: hash, state: state.Key()}
	if !collector.claim(key) {
		return nil
	}
	return r.WalkTreeEntries(hash, func(entry githistory.TreeEntry) error {
		mode := canonicalLegalTreeMode(entry.Mode)
		switch mode {
		case filemode.Dir:
			child, err := state.Enter(entry.Name)
			if err != nil {
				return fmt.Errorf("enter tree %q: %w", entry.Name, err)
			}
			return collectLegalTreeRaw(r, entry.Hash, child, collector)
		case filemode.Regular, filemode.Deprecated, filemode.Executable:
			fileRoles, err := state.Match(entry.Name)
			if err != nil {
				return fmt.Errorf("match file %q: %w", entry.Name, err)
			}
			if fileRoles.Has(roles.Legal) {
				collector.add(entry.Hash)
			}
			return nil
		case filemode.Symlink, filemode.Submodule:
			return nil
		default:
			return fmt.Errorf("unsupported mode %s for %q", mode, entry.Name)
		}
	})
}

func canonicalLegalTreeMode(mode filemode.FileMode) filemode.FileMode {
	switch mode & gitFileTypeMask {
	case filemode.Dir:
		return filemode.Dir
	case filemode.Regular & gitFileTypeMask:
		if mode&0o111 != 0 {
			return filemode.Executable
		}
		return filemode.Regular
	case filemode.Symlink:
		return filemode.Symlink
	default:
		return filemode.Submodule
	}
}
