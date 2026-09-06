package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/object/commitgraph"
)

type historyRootNode struct {
	nodes  []commitgraph.CommitNode
	hashes []plumbing.Hash
}

func (n *historyRootNode) ID() plumbing.Hash { return plumbing.ZeroHash }
func (n *historyRootNode) Tree() (*object.Tree, error) {
	return nil, errors.New("history root has no tree")
}
func (n *historyRootNode) CommitTime() time.Time { return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC) }
func (n *historyRootNode) NumParents() int       { return len(n.nodes) }
func (n *historyRootNode) ParentNodes() commitgraph.CommitNodeIter {
	return &historyRootIter{nodes: n.nodes}
}
func (n *historyRootNode) ParentHashes() []plumbing.Hash { return n.hashes }
func (n *historyRootNode) Generation() uint64            { return math.MaxUint64 }
func (n *historyRootNode) GenerationV2() uint64          { return math.MaxUint64 }
func (n *historyRootNode) Commit() (*object.Commit, error) {
	return nil, errors.New("history root has no commit")
}

func (n *historyRootNode) ParentNode(i int) (commitgraph.CommitNode, error) {
	if i < 0 || i >= len(n.nodes) {
		return nil, object.ErrParentNotFound
	}
	return n.nodes[i], nil
}

type historyRootIter struct {
	nodes []commitgraph.CommitNode
	next  int
}

func (i *historyRootIter) Next() (commitgraph.CommitNode, error) {
	if i.next == len(i.nodes) {
		return nil, io.EOF
	}
	n := i.nodes[i.next]
	i.next++
	return n, nil
}

func (i *historyRootIter) ForEach(visit func(commitgraph.CommitNode) error) error {
	for {
		n, err := i.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := visit(n); err != nil {
			return err
		}
	}
}

func (i *historyRootIter) Close() {}

type shallowCommitNodeIndex struct {
	commitgraph.CommitNodeIndex
	boundary map[plumbing.Hash]bool
}

func (i *shallowCommitNodeIndex) Get(hash plumbing.Hash) (commitgraph.CommitNode, error) {
	n, err := i.CommitNodeIndex.Get(hash)
	if err != nil || !i.boundary[hash] {
		return n, err
	}
	return &shallowCommitNode{CommitNode: n}, nil
}

type shallowCommitNode struct {
	commitgraph.CommitNode
}

func (n *shallowCommitNode) NumParents() int                         { return 0 }
func (n *shallowCommitNode) ParentNodes() commitgraph.CommitNodeIter { return &historyRootIter{} }
func (n *shallowCommitNode) ParentNode(int) (commitgraph.CommitNode, error) {
	return nil, object.ErrParentNotFound
}
func (n *shallowCommitNode) ParentHashes() []plumbing.Hash { return nil }

type historyTreeLoad struct {
	ready chan struct{}
	tree  *object.Tree
	err   error
}

type historyTreeCache struct {
	mu    sync.Mutex
	trees map[plumbing.Hash]*historyTreeLoad
}

func newHistoryTreeCache() *historyTreeCache {
	return &historyTreeCache{trees: make(map[plumbing.Hash]*historyTreeLoad)}
}

func (c *historyTreeCache) load(hash plumbing.Hash, load func() (*object.Tree, error)) (*object.Tree, error) {
	c.mu.Lock()
	entry := c.trees[hash]
	if entry == nil {
		entry = &historyTreeLoad{ready: make(chan struct{})}
		c.trees[hash] = entry
		c.mu.Unlock()
		entry.tree, entry.err = load()
		close(entry.ready)
		return entry.tree, entry.err
	}
	c.mu.Unlock()
	<-entry.ready
	return entry.tree, entry.err
}

func (c *historyTreeCache) release(hash plumbing.Hash) {
	c.mu.Lock()
	delete(c.trees, hash)
	c.mu.Unlock()
}

func walkChangesGoGitStream(r *gogit.Repository, merges bool, visit func(change)) error {
	rootHashes, err := historyRoots(r)
	if err != nil {
		return err
	}
	if len(rootHashes) == 0 {
		return nil
	}
	shallow, err := r.Storer.Shallow()
	if err != nil {
		return err
	}
	boundary := make(map[plumbing.Hash]bool, len(shallow))
	for _, hash := range shallow {
		boundary[hash] = true
	}
	index := &shallowCommitNodeIndex{
		CommitNodeIndex: commitgraph.NewObjectCommitNodeIndex(r.Storer),
		boundary:        boundary,
	}
	root := &historyRootNode{}
	seen := make(map[plumbing.Hash]bool, len(rootHashes))
	for _, hash := range rootHashes {
		if seen[hash] {
			continue
		}
		seen[hash] = true
		node, err := index.Get(hash)
		if err != nil {
			return err
		}
		root.nodes = append(root.nodes, node)
		root.hashes = append(root.hashes, hash)
	}
	iter := commitgraph.NewCommitNodeIterDateOrder(root, nil, nil)
	defer iter.Close()
	if _, err := iter.Next(); err != nil {
		return err
	}
	cache := newHistoryTreeCache()
	if *historyWorkers > 1 {
		return visitHistoryStreamParallel(iter, cache, merges, visit, *historyWorkers)
	}
	for {
		node, err := iter.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if merges || node.NumParents() < 2 {
			if err := visitCommitNodeChanges(context.Background(), node, cache, visit); err != nil {
				return err
			}
		}
		cache.release(node.ID())
	}
}

func visitCommitNodeChanges(ctx context.Context, node commitgraph.CommitNode, cache *historyTreeCache, visit func(change)) error {
	commit, err := node.Commit()
	if err != nil {
		return err
	}
	to, err := cache.load(node.ID(), node.Tree)
	if err != nil {
		return err
	}
	var from *object.Tree
	parents := node.ParentHashes()
	if len(parents) > 0 {
		parent, err := node.ParentNode(0)
		if err != nil {
			return err
		}
		from, err = cache.load(parents[0], parent.Tree)
		if err != nil {
			return err
		}
	}
	changes, err := object.DiffTreeContext(ctx, from, to)
	if err != nil {
		return err
	}
	hash := node.ID().String()
	zero := strings.Repeat("0", len(hash))
	for _, entry := range changes {
		path := entry.To.Name
		if path == "" {
			path = entry.From.Name
		}
		oldOID, newOID := entry.From.TreeEntry.Hash.String(), entry.To.TreeEntry.Hash.String()
		if entry.From.TreeEntry.Mode == 0 {
			oldOID = zero
		}
		if entry.To.TreeEntry.Mode == 0 {
			newOID = zero
		}
		visit(change{
			commit: hash, date: commit.Author.When.Format(time.RFC3339), subject: commitSubject(commit.Message),
			path: path, oldOID: oldOID, newOID: newOID,
			oldMode: fmt.Sprintf("%06o", entry.From.TreeEntry.Mode), newMode: fmt.Sprintf("%06o", entry.To.TreeEntry.Mode),
			merge: len(parents) > 1,
		})
	}
	return nil
}

type historyStreamTask struct {
	node   commitgraph.CommitNode
	skip   bool
	result chan historyResult
}

func visitHistoryStreamParallel(iter commitgraph.CommitNodeIter, cache *historyTreeCache, merges bool, visit func(change), workers int) error {
	ctx, cancel := context.WithCancel(context.Background())
	tasks := make(chan historyStreamTask, workers)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		close(tasks)
		wg.Wait()
	}()
	for range workers {
		wg.Go(func() {
			for task := range tasks {
				var result historyResult
				if !task.skip {
					result.err = visitCommitNodeChanges(ctx, task.node, cache, func(c change) {
						result.changes = append(result.changes, c)
					})
				}
				task.result <- result
			}
		})
	}
	window := historyLookahead * workers
	pending := make([]historyStreamTask, 0, window)
	done := false
	fill := func() error {
		for len(pending) < window && !done {
			node, err := iter.Next()
			if err == io.EOF {
				done = true
				break
			}
			if err != nil {
				return err
			}
			task := historyStreamTask{
				node:   node,
				skip:   !merges && node.NumParents() >= 2,
				result: make(chan historyResult, 1),
			}
			pending = append(pending, task)
			tasks <- task
		}
		return nil
	}
	if err := fill(); err != nil {
		return err
	}
	for len(pending) > 0 {
		task := pending[0]
		result := <-task.result
		if result.err != nil {
			return result.err
		}
		for _, c := range result.changes {
			visit(c)
		}
		cache.release(task.node.ID())
		pending = pending[1:]
		if err := fill(); err != nil {
			return err
		}
	}
	return nil
}

type legalTreeKey struct {
	hash      plumbing.Hash
	inherited bool
}

type legalTreeCollector struct {
	mu    sync.Mutex
	seen  map[legalTreeKey]struct{}
	legal map[string]bool
}

const legalTreeBatchSize = 48

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

var rawObjectReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 8<<10) },
}

var legalFileNamePrefixes = [...]string{
	"licenses",
	"license",
	"licences",
	"licence",
	"copying",
	"mit-license",
	"copyright",
	"unlicense",
	"notices",
	"notice",
}

func legalBlobsGoGit(path string) (map[string]bool, error) {
	r, err := openGoGit(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	roots, err := historyRoots(r)
	if err != nil {
		return nil, err
	}
	shallow, err := r.Storer.Shallow()
	if err != nil {
		return nil, err
	}
	boundary := make(map[plumbing.Hash]bool, len(shallow))
	for _, hash := range shallow {
		boundary[hash] = true
	}
	collector := newLegalTreeCollector()
	if *historyWorkers <= 1 {
		err = visitCommitTrees(r, roots, boundary, func(hash plumbing.Hash) error {
			return collectLegalTreeRaw(r, hash, false, collector)
		})
		return collector.legal, err
	}
	err = collectLegalTreesParallel(r, roots, boundary, collector, *historyWorkers-1)
	return collector.legal, err
}

func visitCommitTrees(r *gogit.Repository, roots []plumbing.Hash, boundary map[plumbing.Hash]bool, visit func(plumbing.Hash) error) error {
	seen := make(map[plumbing.Hash]struct{})
	stack := append([]plumbing.Hash(nil), roots...)
	for len(stack) > 0 {
		last := len(stack) - 1
		hash := stack[last]
		stack = stack[:last]
		if _, ok := seen[hash]; ok {
			continue
		}
		tree, err := readCommitTreeAndParents(r, hash, !boundary[hash], &stack)
		if err != nil {
			return err
		}
		seen[hash] = struct{}{}
		if err := visit(tree); err != nil {
			return err
		}
	}
	return nil
}

func readCommitTreeAndParents(r *gogit.Repository, hash plumbing.Hash, includeParents bool, parents *[]plumbing.Hash) (tree plumbing.Hash, resultErr error) {
	encoded, err := r.Storer.EncodedObject(plumbing.CommitObject, hash)
	if err != nil {
		return tree, err
	}
	if encoded.Type() != plumbing.CommitObject {
		return tree, fmt.Errorf("object %s is %s, want commit", hash, encoded.Type())
	}
	if memory, ok := encoded.(*plumbing.MemoryObject); ok {
		writer := commitLinksWriter{hash: hash, includeParents: includeParents, parents: parents}
		_, err := memory.WriteTo(&writer)
		return writer.tree, err
	}
	reader, err := encoded.Reader()
	if err != nil {
		return tree, err
	}
	defer func() {
		if err := reader.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	buffer := rawObjectReaderPool.Get().(*bufio.Reader)
	buffer.Reset(reader)
	defer func() {
		buffer.Reset(nil)
		rawObjectReaderPool.Put(buffer)
	}()

	line, readErr := buffer.ReadSlice('\n')
	if readErr != nil && (readErr != io.EOF || len(line) == 0) {
		return tree, fmt.Errorf("commit %s tree: %w", hash, readErr)
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	value, ok := bytes.CutPrefix(line, []byte("tree "))
	if !ok {
		return tree, fmt.Errorf("commit %s has no leading tree header", hash)
	}
	tree, err = parseCommitObjectID(value, hash.Size())
	if err != nil {
		return tree, fmt.Errorf("commit %s tree: %w", hash, err)
	}
	if readErr == io.EOF {
		return tree, nil
	}

	for {
		line, readErr = buffer.ReadSlice('\n')
		if readErr != nil && (readErr != io.EOF || len(line) == 0) {
			return tree, fmt.Errorf("commit %s parent: %w", hash, readErr)
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		value, ok = bytes.CutPrefix(line, []byte("parent "))
		if !ok {
			return tree, nil
		}
		parent, err := parseCommitObjectID(value, hash.Size())
		if err != nil {
			return tree, fmt.Errorf("commit %s parent: %w", hash, err)
		}
		if includeParents {
			*parents = append(*parents, parent)
		}
		if readErr == io.EOF {
			return tree, nil
		}
	}
}

type commitLinksWriter struct {
	hash           plumbing.Hash
	tree           plumbing.Hash
	includeParents bool
	parents        *[]plumbing.Hash
}

func (w *commitLinksWriter) Write(data []byte) (int, error) {
	line, rest, _ := bytes.Cut(data, []byte{'\n'})
	value, ok := bytes.CutPrefix(line, []byte("tree "))
	if !ok {
		return 0, fmt.Errorf("commit %s has no leading tree header", w.hash)
	}
	tree, err := parseCommitObjectID(value, w.hash.Size())
	if err != nil {
		return 0, fmt.Errorf("commit %s tree: %w", w.hash, err)
	}
	w.tree = tree
	for len(rest) > 0 {
		line, next, _ := bytes.Cut(rest, []byte{'\n'})
		value, ok = bytes.CutPrefix(line, []byte("parent "))
		if !ok {
			break
		}
		parent, err := parseCommitObjectID(value, w.hash.Size())
		if err != nil {
			return 0, fmt.Errorf("commit %s parent: %w", w.hash, err)
		}
		if w.includeParents {
			*w.parents = append(*w.parents, parent)
		}
		rest = next
	}
	return len(data), nil
}

func parseCommitObjectID(value []byte, size int) (plumbing.Hash, error) {
	if len(value) != hex.EncodedLen(size) {
		return plumbing.ZeroHash, fmt.Errorf("invalid object ID %q", value)
	}
	var decoded objectID
	if _, err := hex.Decode(decoded[:size], value); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("invalid object ID %q: %w", value, err)
	}
	hash, ok := plumbing.FromBytes(decoded[:size])
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("invalid object ID %q", value)
	}
	return hash, nil
}

func collectLegalTreesParallel(r *gogit.Repository, roots []plumbing.Hash, boundary map[plumbing.Hash]bool, collector *legalTreeCollector, workers int) error {
	ctx, cancel := context.WithCancel(context.Background())
	tasks := make(chan legalTreeBatch, workers*2)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for range workers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case batch, ok := <-tasks:
					if !ok {
						return
					}
					for _, hash := range batch.hashes[:batch.count] {
						if err := collectLegalTreeRaw(r, hash, false, collector); err != nil {
							errMu.Lock()
							if firstErr == nil {
								firstErr = err
								cancel()
							}
							errMu.Unlock()
							return
						}
					}
				}
			}
		})
	}
	var batch legalTreeBatch
	enqueue := func() error {
		select {
		case <-ctx.Done():
			errMu.Lock()
			defer errMu.Unlock()
			return firstErr
		case tasks <- batch:
			batch.count = 0
			return nil
		}
	}
	err := visitCommitTrees(r, roots, boundary, func(hash plumbing.Hash) error {
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
	cancel()
	if err != nil {
		return err
	}
	return firstErr
}

func collectLegalTreeRaw(r *gogit.Repository, hash plumbing.Hash, inherited bool, collector *legalTreeCollector) (resultErr error) {
	key := legalTreeKey{hash: hash, inherited: inherited}
	if !collector.claim(key) {
		return nil
	}
	object, err := r.Storer.EncodedObject(plumbing.TreeObject, hash)
	if err != nil {
		return err
	}
	if object.Type() != plumbing.TreeObject {
		return fmt.Errorf("object %s is %s, want tree", hash, object.Type())
	}
	if memory, ok := object.(*plumbing.MemoryObject); ok {
		writer := legalTreeWriter{repository: r, hash: hash, inherited: inherited, collector: collector}
		_, err := memory.WriteTo(writer)
		return err
	}
	reader, err := object.Reader()
	if err != nil {
		return err
	}
	defer func() {
		if err := reader.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	buffer := rawObjectReaderPool.Get().(*bufio.Reader)
	buffer.Reset(reader)
	defer func() {
		buffer.Reset(nil)
		rawObjectReaderPool.Put(buffer)
	}()
	for {
		modeBytes, err := buffer.ReadSlice(' ')
		if err == io.EOF && len(modeBytes) == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tree %s mode: %w", hash, err)
		}
		mode, err := filemode.FromBytes(modeBytes[:len(modeBytes)-1])
		if err != nil {
			return fmt.Errorf("tree %s mode: %w", hash, err)
		}
		mode = canonicalLegalTreeMode(mode)
		name, err := buffer.ReadSlice(0)
		if err != nil {
			return fmt.Errorf("tree %s name: %w", hash, err)
		}
		name = name[:len(name)-1]
		if len(name) == 0 {
			return fmt.Errorf("tree %s has an empty filename", hash)
		}
		oidBytes, err := buffer.Peek(hash.Size())
		if err != nil {
			return fmt.Errorf("tree %s object ID: %w", hash, err)
		}
		oid, ok := plumbing.FromBytes(oidBytes)
		if !ok {
			return fmt.Errorf("tree %s has an invalid object ID", hash)
		}
		if _, err := buffer.Discard(hash.Size()); err != nil {
			return fmt.Errorf("tree %s object ID: %w", hash, err)
		}
		switch mode {
		case filemode.Dir:
			childLegal := inherited || isLegalDirectoryBytes(name)
			if err := collectLegalTreeRaw(r, oid, childLegal, collector); err != nil {
				return err
			}
		case filemode.Regular, filemode.Deprecated, filemode.Executable:
			if inherited || isLegalFileNameBytes(name) {
				collector.add(oid)
			}
		case filemode.Symlink, filemode.Submodule:
		default:
			return fmt.Errorf("unsupported mode %s for %q", mode, name)
		}
	}
}

type legalTreeWriter struct {
	repository *gogit.Repository
	hash       plumbing.Hash
	inherited  bool
	collector  *legalTreeCollector
}

func (w legalTreeWriter) Write(data []byte) (int, error) {
	size := len(data)
	for len(data) > 0 {
		modeEnd := bytes.IndexByte(data, ' ')
		if modeEnd < 0 {
			return 0, fmt.Errorf("tree %s mode: %w", w.hash, io.ErrUnexpectedEOF)
		}
		mode, err := filemode.FromBytes(data[:modeEnd])
		if err != nil {
			return 0, fmt.Errorf("tree %s mode: %w", w.hash, err)
		}
		mode = canonicalLegalTreeMode(mode)
		data = data[modeEnd+1:]
		nameEnd := bytes.IndexByte(data, 0)
		if nameEnd < 0 {
			return 0, fmt.Errorf("tree %s name: %w", w.hash, io.ErrUnexpectedEOF)
		}
		name := data[:nameEnd]
		if len(name) == 0 {
			return 0, fmt.Errorf("tree %s has an empty filename", w.hash)
		}
		data = data[nameEnd+1:]
		if len(data) < w.hash.Size() {
			return 0, fmt.Errorf("tree %s object ID: %w", w.hash, io.ErrUnexpectedEOF)
		}
		oid, ok := plumbing.FromBytes(data[:w.hash.Size()])
		if !ok {
			return 0, fmt.Errorf("tree %s has an invalid object ID", w.hash)
		}
		data = data[w.hash.Size():]
		switch mode {
		case filemode.Dir:
			childLegal := w.inherited || isLegalDirectoryBytes(name)
			if err := collectLegalTreeRaw(w.repository, oid, childLegal, w.collector); err != nil {
				return 0, err
			}
		case filemode.Regular, filemode.Deprecated, filemode.Executable:
			if w.inherited || isLegalFileNameBytes(name) {
				w.collector.add(oid)
			}
		case filemode.Symlink, filemode.Submodule:
		default:
			return 0, fmt.Errorf("unsupported mode %s for %q", mode, name)
		}
	}
	return size, nil
}

func canonicalLegalTreeMode(mode filemode.FileMode) filemode.FileMode {
	switch mode & 0o170000 {
	case 0o040000:
		return filemode.Dir
	case 0o100000:
		if mode&0o111 != 0 {
			return filemode.Executable
		}
		return filemode.Regular
	case 0o120000:
		return filemode.Symlink
	default:
		return filemode.Submodule
	}
}

func isLegalDirectoryBytes(name []byte) bool {
	return equalFoldASCII(name, "license") ||
		equalFoldASCII(name, "licenses") ||
		equalFoldASCII(name, "licence") ||
		equalFoldASCII(name, "licences")
}

func isLegalFileNameBytes(name []byte) bool {
	for _, prefix := range legalFileNamePrefixes {
		if len(name) < len(prefix) || !equalFoldASCII(name[:len(prefix)], prefix) {
			continue
		}
		if len(name) == len(prefix) {
			return true
		}
		switch name[len(prefix)] {
		case '.', '-', '_':
			return true
		}
	}
	return false
}

func equalFoldASCII(input []byte, pattern string) bool {
	if len(input) != len(pattern) {
		return false
	}
	for index, char := range input {
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}
		if char != pattern[index] {
			return false
		}
	}
	return true
}
