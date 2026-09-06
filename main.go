package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/licenses"
	"github.com/go-git/go-git/v6/plumbing"
)

const (
	goGitBackend              = "gogit"
	defaultMaxBlobSize        = 1 << 20
	defaultLegalBlobSize      = 8 << 20
	initialBlobCapacity       = 1 << 16
	initialExpressionCapacity = 128
	readerBufferSize          = 1 << 16
	jobQueueSize              = 256
	jobBatchSize              = 64
	jobBatchBytes             = 8 << 20
	usageExitCode             = 2
	catHeaderFields           = 3
	expressionDisplayLimit    = 20
	zeroOID                   = "0000000000000000000000000000000000000000"
	zeroOIDSHA256             = "0000000000000000000000000000000000000000000000000000000000000000"
)

var (
	cpuProfile    = flag.String("cpuprofile", "", "write CPU profile of the blob scan to file")
	memProfile    = flag.String("memprofile", "", "write heap profile after the blob scan to file")
	backend       = flag.String("backend", "git", "backend: git (subprocesses) or gogit (Git-free)")
	maxBlobSize   = flag.Int64("max-blob-size", defaultMaxBlobSize, "skip blobs larger than this many bytes")
	readers       = flag.Int("readers", 0, "blob readers (0 = GOMAXPROCS for git, 1 for gogit)")
	legalBlobSize = flag.Int64("max-legal-blob-size", defaultLegalBlobSize, "size limit for blobs used at legal paths")
	details       = flag.Bool("details", false, "print individual history changes")
	group         = flag.String("group", groupAll, "history group: all, root, legal, other")
	monthly       = flag.Bool("monthly", false, "write monthly history counts as CSV")
)

func main() {
	flag.Usage = usage
	flag.Parse()
	if err := configureGoGitZlib(); err != nil {
		fatal(err)
	}
	args := flag.Args()
	if *maxBlobSize < 0 || *legalBlobSize < 0 {
		fatal(fmt.Errorf("blob size limits must be non-negative"))
	}
	if *goGitObjectBuffer < 0 {
		fatal(fmt.Errorf("go-git object buffer must be non-negative"))
	}
	if *goGitObjectBatch <= 0 {
		fatal(fmt.Errorf("go-git object batch must be positive"))
	}
	if !slices.Contains([]string{groupAll, groupRoot, groupLegal, groupOther}, *group) {
		fatal(fmt.Errorf("unknown history group %q", *group))
	}
	if *monthly && *details {
		fatal(fmt.Errorf("-monthly and -details cannot be combined"))
	}
	if len(args) == 0 {
		usage()
		os.Exit(usageExitCode)
	}
	cmd, rest := args[0], args[1:]
	repo := "."
	if len(rest) > 0 {
		repo = rest[0]
	}
	switch cmd {
	case "scan":
		if err := scan(repo); err != nil {
			fatal(err)
		}
	case "log":
		if err := logCmd(repo); err != nil {
			fatal(err)
		}
	default:
		usage()
		os.Exit(usageExitCode)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: git spdx [options] <scan|log> [repo]")
	flag.PrintDefaults()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "git-spdx:", err)
	os.Exit(1)
}

type blobResult struct {
	expressions []string
	skipped     skipReason
	spdx        bool
}

type skipReason uint8

const (
	notSkipped skipReason = iota
	skippedSize
	skippedBinary
	skippedError
)

func (s skipReason) String() string {
	switch s {
	case skippedSize:
		return "size"
	case skippedBinary:
		return "binary"
	case skippedError:
		return "error"
	default:
		return "scanned"
	}
}

// Each index belongs to one repository with one object format.
type objectID [sha256.Size]byte

func parseOID(s string) (objectID, error) {
	var oid objectID
	if len(s) != hex.EncodedLen(sha1.Size) && len(s) != hex.EncodedLen(sha256.Size) {
		return oid, fmt.Errorf("invalid object ID %q", s)
	}
	_, err := hex.Decode(oid[:], []byte(s))
	return oid, err
}

type index struct {
	mu       sync.RWMutex
	blobs    map[objectID]blobResult
	interned map[string]string
	skipped  int
	err      error
}

func newIndex() *index {
	return &index{blobs: make(map[objectID]blobResult, initialBlobCapacity), interned: make(map[string]string, initialExpressionCapacity)}
}

func (x *index) get(oid string) (blobResult, bool) {
	key, err := parseOID(oid)
	if err != nil {
		return blobResult{skipped: skippedError}, true
	}
	x.mu.RLock()
	r, ok := x.blobs[key]
	x.mu.RUnlock()
	return r, ok
}

// Empty successful results are implicit after a completed scan.
func (x *index) put(oid string, r blobResult) {
	if len(r.expressions) == 0 && r.skipped == notSkipped && !r.spdx {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	key, err := parseOID(oid)
	if err != nil {
		x.err = err
		return
	}
	if r.skipped != notSkipped {
		x.skipped++
	}
	for i, e := range r.expressions {
		if s, ok := x.interned[e]; ok {
			r.expressions[i] = s
		} else {
			x.interned[e] = e
		}
	}
	x.blobs[key] = r
}

// scan matches every blob reachable from any ref and reports throughput.
func scan(repo string) error {
	ctx := context.Background()

	t0 := time.Now()
	m, err := licenses.New()
	if err != nil {
		return err
	}
	loadDur := time.Since(t0)

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = f.Close()
		}()
	}

	idx := newIndex()
	stats, err := scanBlobs(ctx, repo, m, idx)
	if err != nil {
		return err
	}

	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			return err
		}
		runtime.GC()
		if err := pprof.WriteHeapProfile(f); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	fmt.Printf("matcher load        %8s\n", loadDur.Round(time.Millisecond))
	fmt.Printf("legal discovery     %8s\n", stats.legalElapsed.Round(time.Millisecond))
	fmt.Printf("blob processing     %8s\n", stats.blobElapsed.Round(time.Millisecond))
	fmt.Printf("blob scan           %8s\n", stats.elapsed.Round(time.Millisecond))
	fmt.Printf("total               %8s\n", (loadDur + stats.elapsed).Round(time.Millisecond))
	fmt.Printf("blobs seen          %8d\n", stats.total)
	fmt.Printf("blobs matched       %8d\n", stats.matched)
	fmt.Printf("blobs with hits     %8d\n", stats.hits)
	fmt.Printf("skipped: size       %8d\n", stats.skipSize)
	fmt.Printf("skipped: binary     %8d\n", stats.skipBinary)
	fmt.Printf("skipped: error      %8d\n", stats.skipError)
	fmt.Printf("bytes matched       %8s\n", human(stats.bytes))
	if stats.matched > 0 {
		perBlob := stats.elapsed / time.Duration(stats.matched)
		fmt.Printf("mean per blob       %8s\n", perBlob.Round(time.Microsecond))
	}
	fmt.Println()
	fmt.Println("expressions across all history:")
	printExprCounts(idx)
	return nil
}

type scanStats struct {
	total, matched, hits            int
	skipSize, skipBinary, skipError int
	bytes                           int64
	elapsed, legalElapsed           time.Duration
	blobElapsed                     time.Duration
}

type job struct {
	oid  string
	data []byte
}

func scanBlobs(ctx context.Context, repo string, m *licenses.Matcher, idx *index) (scanStats, error) {
	return scanBlobsWith(ctx, repo, m, idx, feederFor(*backend))
}

func scanBlobsWith(ctx context.Context, repo string, m *licenses.Matcher, idx *index, feed feeder) (scanStats, error) {
	t0 := time.Now()
	legal, err := legalBlobs(repo)
	if err != nil {
		return scanStats{}, err
	}
	legalElapsed := time.Since(t0)
	reportBenchmarkPhase("legal", t0)
	stats, err := scanBlobsWithLegal(ctx, repo, m, idx, feed, legal)
	stats.legalElapsed = legalElapsed
	stats.elapsed = time.Since(t0)
	return stats, err
}

func scanBlobsWithLegal(ctx context.Context, repo string, m *licenses.Matcher, idx *index, feed feeder, legal map[string]bool) (scanStats, error) {
	var stats scanStats
	var err error
	t0 := time.Now()
	limit := func(oid string) int64 {
		if legal[oid] {
			return max(*maxBlobSize, *legalBlobSize)
		}
		return *maxBlobSize
	}

	jobs := make(chan job, jobQueueSize)
	var wg sync.WaitGroup
	var mu sync.Mutex
	workers := runtime.GOMAXPROCS(0)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				r := matchBlob(ctx, m, j.data)
				idx.put(j.oid, r)
				mu.Lock()
				stats.matched++
				stats.bytes += int64(len(j.data))
				if len(r.expressions) > 0 {
					stats.hits++
				}
				if r.skipped == skippedBinary {
					stats.skipBinary++
				}
				if r.skipped == skippedError {
					stats.skipError++
				}
				if stats.matched%200_000 == 0 {
					fmt.Fprintf(os.Stderr, "  %d blobs, %s, %s\n",
						stats.matched, human(stats.bytes), time.Since(t0).Round(time.Second))
				}
				mu.Unlock()
			}
		}()
	}

	skip := func(oid string) {
		idx.put(oid, blobResult{skipped: skippedSize})
		mu.Lock()
		stats.skipSize++
		mu.Unlock()
	}

	stats.total, err = feed(repo, jobs, skip, limit)
	reportBenchmarkPhase("feed", t0)
	close(jobs)
	wg.Wait()
	reportBenchmarkPhase("match", t0)
	if err != nil {
		return stats, err
	}
	if idx.err != nil {
		return stats, idx.err
	}
	stats.blobElapsed = time.Since(t0)
	stats.elapsed = stats.blobElapsed
	return stats, nil
}

func reportBenchmarkPhase(name string, started time.Time) {
	if os.Getenv("GITSPDX_BENCH_PHASES") != "1" {
		return
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	fmt.Fprintf(os.Stderr, "bench-phase name=%s elapsed=%s heap_alloc=%d heap_sys=%d\n",
		name, time.Since(started).Round(time.Millisecond), memory.HeapAlloc, memory.HeapSys)
}

type feeder func(repo string, jobs chan<- job, skip func(string), limit func(string) int64) (int, error)

type listedBlob struct {
	oid  string
	size int64
}

func feederFor(name string) feeder {
	if name == goGitBackend {
		return feedBlobsGoGit
	}
	return feedBlobsCatFile
}

func feedBlobsCatFile(repo string, jobs chan<- job, skip func(string), limit func(string) int64) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shards := *readers
	if shards <= 0 {
		shards = runtime.GOMAXPROCS(0)
	}
	shards = max(1, shards)
	blobs := make(chan listedBlob, jobQueueSize)
	var wg sync.WaitGroup
	errs := make(chan error, shards)
	for s := 0; s < shards; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cat, err := newCatFile(repo)
			if err != nil {
				errs <- err
				cancel()
				return
			}
			defer cat.close()
			for blob := range blobs {
				cap := limit(blob.oid)
				typ, size, data, err := cat.read(blob.oid, cap)
				if err != nil {
					errs <- err
					cancel()
					return
				}
				if typ != "blob" {
					continue
				}
				if int64(size) > cap {
					skip(blob.oid)
					continue
				}
				jobs <- job{oid: blob.oid, data: data}
			}
		}()
	}
	total, err := listBlobs(ctx, repo, func(blob listedBlob) error {
		if blob.size > limit(blob.oid) {
			skip(blob.oid)
			return nil
		}
		select {
		case blobs <- blob:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(blobs)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return total, e
		}
	}
	return total, err
}

func feedBlobsGoGit(repo string, jobs chan<- job, skip func(string), limit func(string) int64) (int, error) {
	if *readers > 1 {
		return feedBlobsGoGitParallel(repo, jobs, skip, limit, *readers)
	}
	return feedBlobsGoGitSerial(repo, jobs, skip, limit)
}

func feedBlobsGoGitSerial(repo string, jobs chan<- job, skip func(string), limit func(string) int64) (int, error) {
	r, err := openGoGit(repo)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	iter, err := r.Storer.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	total := 0
	batch := make([]job, 0, jobBatchSize)
	batchBytes := 0
	flush := func() {
		for _, j := range batch {
			jobs <- j
		}
		batch = batch[:0]
		batchBytes = 0
	}
	for {
		obj, err := iter.Next()
		if err == io.EOF {
			flush()
			return total, nil
		}
		if err != nil {
			return total, err
		}
		total++
		oid := obj.Hash().String()
		if obj.Size() > limit(oid) {
			skip(oid)
			continue
		}
		rd, err := obj.Reader()
		if err != nil {
			return total, err
		}
		data := make([]byte, obj.Size())
		_, err = io.ReadFull(rd, data)
		closeErr := rd.Close()
		if err != nil {
			return total, err
		}
		if closeErr != nil {
			return total, closeErr
		}
		if len(batch) > 0 && batchBytes+len(data) > jobBatchBytes {
			flush()
		}
		batch = append(batch, job{oid: oid, data: data})
		batchBytes += len(data)
		if len(batch) == cap(batch) {
			flush()
		}
	}
}

func matchBlob(ctx context.Context, m *licenses.Matcher, data []byte) blobResult {
	if bytes.IndexByte(data, 0) >= 0 {
		return blobResult{skipped: skippedBinary}
	}
	res, err := m.Match(ctx, data)
	if err != nil {
		return blobResult{skipped: skippedError}
	}
	exprs := make([]string, 0, len(res.Detections))
	for _, d := range res.Detections {
		exprs = append(exprs, d.Expression)
	}
	sort.Strings(exprs)
	return blobResult{expressions: exprs, spdx: len(res.SPDXDeclarations) > 0}
}

// listBlobs includes unreachable objects in the object store.
func listBlobs(ctx context.Context, repo string, visit func(listedBlob) error) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "cat-file",
		"--batch-all-objects", "--batch-check=%(objecttype) %(objectname) %(objectsize)",
		"--unordered")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	total := 0
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "blob" {
			continue
		}
		size, parseErr := strconv.ParseInt(fields[2], 10, 64)
		if parseErr != nil {
			err = fmt.Errorf("parse blob size %q: %w", fields[2], parseErr)
			break
		}
		total++
		if err = visit(listedBlob{oid: fields[1], size: size}); err != nil {
			break
		}
	}
	if err == nil {
		err = scanner.Err()
	}
	if err != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if err != nil {
		return total, err
	}
	return total, waitErr
}

type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func newCatFile(repo string) (*catFile, error) {
	cmd := exec.Command("git", "-C", repo, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, readerBufferSize)}, nil
}

func (c *catFile) read(oid string, limit int64) (typ string, size int, data []byte, err error) {
	if _, err = fmt.Fprintln(c.in, oid); err != nil {
		return
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) < catHeaderFields {
		err = fmt.Errorf("cat-file header: %q", header)
		return
	}
	typ = parts[1]
	size, err = strconv.Atoi(parts[2])
	if err != nil {
		return
	}
	if int64(size) <= limit {
		data = make([]byte, size)
		if _, err = io.ReadFull(c.out, data); err != nil {
			return
		}
	} else {
		if _, err = io.CopyN(io.Discard, c.out, int64(size)); err != nil {
			return
		}
	}
	// trailing LF
	_, err = c.out.Discard(1)
	return
}

func (c *catFile) close() {
	_ = c.in.Close()
	_ = c.cmd.Wait()
}

func lookup(idx *index, oid string) blobResult {
	if oid == "" || oid == zeroOID || oid == zeroOIDSHA256 {
		return blobResult{}
	}
	if r, ok := idx.get(oid); ok {
		return r
	}
	return blobResult{}
}

func equalExprs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func printExprCounts(idx *index) {
	counts := map[string]int{}
	for _, r := range idx.blobs {
		for _, e := range r.expressions {
			counts[e]++
		}
	}
	type kv struct {
		k string
		v int
	}
	var s []kv
	for k, v := range counts {
		s = append(s, kv{k, v})
	}
	sort.Slice(s, func(i, j int) bool {
		if s[i].v != s[j].v {
			return s[i].v > s[j].v
		}
		return s[i].k < s[j].k
	})
	for i, e := range s {
		if i >= expressionDisplayLimit {
			fmt.Printf("  ... %d more\n", len(s)-expressionDisplayLimit)
			break
		}
		fmt.Printf("  %6d  %s\n", e.v, e.k)
	}
}

func human(n int64) string {
	units := []string{"B", "KB", "MB", "GB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
