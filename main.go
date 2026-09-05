package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/licenses"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const (
	defaultMaxBlobSize = 1 << 20
	zeroOID            = "0000000000000000000000000000000000000000"
	zeroOIDSHA256      = "0000000000000000000000000000000000000000000000000000000000000000"
)

var (
	cpuProfile  = flag.String("cpuprofile", "", "write CPU profile of the blob scan to file")
	memProfile  = flag.String("memprofile", "", "write heap profile after the blob scan to file")
	backend     = flag.String("backend", "git", "object reader: git (cat-file) or gogit")
	maxBlobSize = flag.Int64("max-blob-size", defaultMaxBlobSize, "skip blobs larger than this many bytes")
	readers     = flag.Int("readers", 0, "cat-file reader processes (0 = GOMAXPROCS)")
)

func main() {
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
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
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: git spdx <scan|log> [repo]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "git-spdx:", err)
	os.Exit(1)
}

type blobResult struct {
	expressions []string
	skipped     string
}

type index struct {
	mu       sync.RWMutex
	blobs    map[string]blobResult
	interned map[string]string
	skipped  int
}

func (x *index) get(oid string) (blobResult, bool) {
	x.mu.RLock()
	r, ok := x.blobs[oid]
	x.mu.RUnlock()
	return r, ok
}

// put stores only blobs with detections; absence means scanned and empty.
// Expression strings are interned so repeated values share storage.
func (x *index) put(oid string, r blobResult) {
	if r.skipped != "" {
		x.mu.Lock()
		x.skipped++
		x.mu.Unlock()
		return
	}
	if len(r.expressions) == 0 {
		return
	}
	x.mu.Lock()
	for i, e := range r.expressions {
		if s, ok := x.interned[e]; ok {
			r.expressions[i] = s
		} else {
			x.interned[e] = e
		}
	}
	x.blobs[oid] = r
	x.mu.Unlock()
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
			f.Close()
		}()
	}

	idx := &index{blobs: make(map[string]blobResult, 1<<16), interned: make(map[string]string, 128)}
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
		f.Close()
	}

	fmt.Printf("matcher load        %8s\n", loadDur.Round(time.Millisecond))
	fmt.Printf("blob scan           %8s\n", stats.elapsed.Round(time.Millisecond))
	fmt.Printf("total               %8s\n", (loadDur + stats.elapsed).Round(time.Millisecond))
	fmt.Printf("blobs seen          %8d\n", stats.total)
	fmt.Printf("blobs matched       %8d\n", stats.matched)
	fmt.Printf("blobs with hits     %8d\n", stats.hits)
	fmt.Printf("skipped: size       %8d\n", stats.skipSize)
	fmt.Printf("skipped: binary     %8d\n", stats.skipBinary)
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
	total, matched, hits int
	skipSize, skipBinary int
	bytes                int64
	elapsed              time.Duration
}

type job struct {
	oid  string
	data []byte
}

func scanBlobs(ctx context.Context, repo string, m *licenses.Matcher, idx *index) (scanStats, error) {
	return scanBlobsWith(ctx, repo, m, idx, feederFor(*backend))
}

func scanBlobsWith(ctx context.Context, repo string, m *licenses.Matcher, idx *index, feed feeder) (scanStats, error) {
	var stats scanStats
	t0 := time.Now()

	jobs := make(chan job, 256)
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
				if r.skipped == "binary" {
					stats.skipBinary++
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
		idx.put(oid, blobResult{skipped: "size"})
		mu.Lock()
		stats.skipSize++
		mu.Unlock()
	}

	var err error
	stats.total, err = feed(repo, jobs, skip)
	close(jobs)
	if err != nil {
		return stats, err
	}
	wg.Wait()
	stats.elapsed = time.Since(t0)
	return stats, nil
}

type feeder func(repo string, jobs chan<- job, skip func(string)) (int, error)

func feederFor(name string) feeder {
	if name == "gogit" {
		return feedBlobsGoGit
	}
	return feedBlobsCatFile
}

func feedBlobsCatFile(repo string, jobs chan<- job, skip func(string)) (int, error) {
	oids, err := listBlobs(repo)
	if err != nil {
		return 0, err
	}
	shards := *readers
	if shards <= 0 {
		shards = runtime.GOMAXPROCS(0)
	}
	shards = max(1, min(shards, len(oids)))
	var wg sync.WaitGroup
	errs := make(chan error, shards)
	for s := 0; s < shards; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			cat, err := newCatFile(repo)
			if err != nil {
				errs <- err
				return
			}
			defer cat.close()
			for i := s; i < len(oids); i += shards {
				typ, size, data, err := cat.read(oids[i])
				if err != nil {
					errs <- err
					return
				}
				if typ != "blob" {
					continue
				}
				if int64(size) > *maxBlobSize {
					skip(oids[i])
					continue
				}
				jobs <- job{oid: oids[i], data: data}
			}
		}(s)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return len(oids), e
		}
	}
	return len(oids), nil
}

func feedBlobsGoGit(repo string, jobs chan<- job, skip func(string)) (int, error) {
	r, err := gogit.PlainOpen(repo)
	if err != nil {
		return 0, err
	}
	iter, err := r.Storer.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	total := 0
	for {
		obj, err := iter.Next()
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		total++
		oid := obj.Hash().String()
		if obj.Size() > *maxBlobSize {
			skip(oid)
			continue
		}
		rd, err := obj.Reader()
		if err != nil {
			return total, err
		}
		data := make([]byte, obj.Size())
		_, err = io.ReadFull(rd, data)
		rd.Close()
		if err != nil {
			return total, err
		}
		jobs <- job{oid: oid, data: data}
	}
}

func matchBlob(ctx context.Context, m *licenses.Matcher, data []byte) blobResult {
	if bytes.IndexByte(data, 0) >= 0 {
		return blobResult{skipped: "binary"}
	}
	res, err := m.Match(ctx, data)
	if err != nil {
		return blobResult{skipped: "error"}
	}
	if len(res.Detections) == 0 {
		return blobResult{}
	}
	exprs := make([]string, 0, len(res.Detections))
	for _, d := range res.Detections {
		exprs = append(exprs, d.Expression)
	}
	sort.Strings(exprs)
	return blobResult{expressions: exprs}
}

// listBlobs returns every unique blob OID reachable from any ref.
func listBlobs(repo string) ([]string, error) {
	cmd := exec.Command("git", "-C", repo, "cat-file",
		"--batch-all-objects", "--batch-check=%(objecttype) %(objectname)",
		"--unordered")
	out, err := cmd.Output()
	if err != nil {
		return nil, gitErr("cat-file --batch-all-objects", err)
	}
	var oids []string
	for _, line := range bytes.Split(out, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("blob ")) {
			continue
		}
		oids = append(oids, string(line[5:]))
	}
	return oids, nil
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
	return &catFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<16)}, nil
}

func (c *catFile) read(oid string) (typ string, size int, data []byte, err error) {
	if _, err = fmt.Fprintln(c.in, oid); err != nil {
		return
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) < 3 {
		err = fmt.Errorf("cat-file header: %q", header)
		return
	}
	typ = parts[1]
	size, err = strconv.Atoi(parts[2])
	if err != nil {
		return
	}
	if int64(size) <= *maxBlobSize {
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
	c.in.Close()
	c.cmd.Wait()
}

// logCmd walks all commits and reports where a path's detected expressions changed.
func logCmd(repo string) error {
	ctx := context.Background()
	m, err := licenses.New()
	if err != nil {
		return err
	}
	idx := &index{blobs: make(map[string]blobResult, 1<<16), interned: make(map[string]string, 128)}
	if _, err := scanBlobs(ctx, repo, m, idx); err != nil {
		return err
	}

	if out, _ := exec.Command("git", "-C", repo, "rev-parse", "--is-shallow-repository").Output(); bytes.HasPrefix(out, []byte("true")) {
		fmt.Fprintln(os.Stderr, "git-spdx: warning: shallow clone; grafted commits will show every file as added")
	}

	cmd := exec.Command("git", "-C", repo, "log", "--all", "--date-order",
		"--no-abbrev", "--raw", "--no-renames",
		"--format=%x00%H%x00%aI%x00%s")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return gitErr("log --raw", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var commit, date, subject string
	printed := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "\x00") {
			f := strings.SplitN(line[1:], "\x00", 3)
			commit, date, subject = f[0], f[1], f[2]
			printed = false
			continue
		}
		if !strings.HasPrefix(line, ":") {
			continue
		}
		oldOID, newOID, path, ok := parseRaw(line)
		if !ok {
			continue
		}
		before := lookup(idx, oldOID)
		after := lookup(idx, newOID)
		if equalExprs(before, after) {
			continue
		}
		if !printed {
			fmt.Printf("%s  %s  %s\n", commit[:12], date[:10], subject)
			printed = true
		}
		fmt.Printf("  %s\n", path)
		if len(before) > 0 {
			fmt.Printf("    - %s\n", strings.Join(before, ", "))
		}
		if len(after) > 0 {
			fmt.Printf("    + %s\n", strings.Join(after, ", "))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("git log --raw: %w", err)
	}
	return cmd.Wait()
}

func parseRaw(line string) (oldOID, newOID, path string, ok bool) {
	// :100644 100644 <old> <new> M\t<path>
	tab := strings.IndexByte(line, '\t')
	if tab < 0 {
		return
	}
	f := strings.Fields(line[:tab])
	if len(f) < 5 {
		return
	}
	return f[2], f[3], line[tab+1:], true
}

func lookup(idx *index, oid string) []string {
	if oid == "" || oid == zeroOID || oid == zeroOIDSHA256 {
		return nil
	}
	if r, ok := idx.get(oid); ok {
		return r.expressions
	}
	return nil
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
	sort.Slice(s, func(i, j int) bool { return s[i].v > s[j].v })
	for i, e := range s {
		if i >= 20 {
			fmt.Printf("  ... %d more\n", len(s)-20)
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

func gitErr(what string, err error) error {
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Errorf("git %s: %s", what, bytes.TrimSpace(ee.Stderr))
	}
	return fmt.Errorf("git %s: %w", what, err)
}
