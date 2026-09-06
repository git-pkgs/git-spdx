package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/git-pkgs/licenses"
)

const (
	groupAll   = "all"
	groupRoot  = "root"
	groupLegal = "legal"
	groupOther = "other"
)

type change struct {
	commit, date, subject string
	oldOID, newOID, path  string
	oldMode, newMode      string
}

func walkChanges(repo string, merges bool, visit func(change)) error {
	mergeMode := "off"
	if merges {
		mergeMode = "first-parent"
	}
	cmd := exec.Command("git", "-C", repo, "log", "--all", "--date-order",
		"--root", "--no-abbrev", "--raw", "--no-renames", "-z",
		"--diff-merges="+mergeMode, "--format=%x01%H%x00%aI%x00%s")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	err = readChanges(stdout, visit)
	if err != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if err != nil {
		return fmt.Errorf("read history: %w", err)
	}
	return waitErr
}

func readChanges(input io.Reader, visit func(change)) error {
	r := bufio.NewReaderSize(input, readerBufferSize)
	field := func() (string, error) {
		s, err := r.ReadString(0)
		return strings.TrimSuffix(s, "\x00"), err
	}
	var c change
	for {
		token, err := field()
		if err == io.EOF && strings.TrimSpace(token) == "" {
			return nil
		}
		if err != nil {
			return err
		}
		token = strings.TrimLeft(token, "\n")
		if strings.HasPrefix(token, "\x01") {
			c.commit = token[1:]
			if c.date, err = field(); err != nil {
				return err
			}
			if c.subject, err = field(); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(token, ":") {
			continue
		}
		fields := strings.Fields(token[1:])
		if len(fields) != 5 || c.commit == "" {
			return fmt.Errorf("invalid raw change %q", token)
		}
		c.oldMode, c.newMode = fields[0], fields[1]
		c.oldOID, c.newOID = fields[2], fields[3]
		if c.path, err = field(); err != nil {
			return err
		}
		visit(c)
	}
}

func pathGroup(path string) string {
	if len(licenses.LegalFileRoles(path)) == 0 {
		return groupOther
	}
	if !strings.Contains(path, "/") {
		return groupRoot
	}
	return groupLegal
}

func legalBlobs(repo string) (map[string]bool, error) {
	legal := make(map[string]bool)
	err := walkChanges(repo, true, func(c change) {
		if pathGroup(c.path) == groupOther {
			return
		}
		if c.oldMode != "000000" && c.oldMode != "160000" {
			legal[c.oldOID] = true
		}
		if c.newMode != "000000" && c.newMode != "160000" {
			legal[c.newOID] = true
		}
	})
	return legal, err
}

type historyTotals struct {
	commits, expressions, additions, deletions, incomplete int
	spdxAdded, spdxRemoved                                 int
	lastCommit                                             string
}

func logCmd(repo string) error {
	m, err := licenses.New()
	if err != nil {
		return err
	}
	idx := newIndex()
	if _, err := scanBlobs(context.Background(), repo, m, idx); err != nil {
		return err
	}
	if out, _ := exec.Command("git", "-C", repo, "rev-parse", "--is-shallow-repository").Output(); bytes.HasPrefix(out, []byte("true")) {
		fmt.Fprintln(os.Stderr, "git-spdx: warning: shallow clone; grafted commits will show every file as added")
	}
	totals := map[string]*historyTotals{groupRoot: {}, groupLegal: {}, groupOther: {}}
	months := make(map[string]*historyTotals)
	lastPrinted := ""
	err = walkChanges(repo, false, func(c change) {
		role := pathGroup(c.path)
		if *group != groupAll && *group != role {
			return
		}
		if c.oldMode == "160000" || c.newMode == "160000" {
			return
		}
		before, after := lookup(idx, c.oldOID), lookup(idx, c.newOID)
		incomplete := before.skipped != notSkipped || after.skipped != notSkipped
		expressions := !equalExprs(before.expressions, after.expressions)
		if !incomplete && !expressions && before.spdx == after.spdx {
			return
		}
		t := totals[role]
		kind := t.record(c, before, after)
		if *monthly {
			key := c.date[:7] + "," + role
			if months[key] == nil {
				months[key] = &historyTotals{}
			}
			months[key].record(c, before, after)
		}
		if *details {
			if lastPrinted != c.commit {
				fmt.Printf("%s  %s  %s\n", c.commit[:12], c.date[:10], c.subject)
				lastPrinted = c.commit
			}
			printChange(c, role, kind, before, after)
		}
	})
	if err != nil {
		return err
	}
	if *monthly {
		return printMonthly(months)
	}
	for _, role := range []string{groupRoot, groupLegal, groupOther} {
		if *group != groupAll && *group != role {
			continue
		}
		t := totals[role]
		fmt.Printf("%s: %d commits; %d expression changes; %d file additions; %d file deletions; %d incomplete comparisons; %d SPDX declarations added; %d removed\n",
			role, t.commits, t.expressions, t.additions, t.deletions, t.incomplete, t.spdxAdded, t.spdxRemoved)
	}
	return nil
}

func printChange(c change, role, kind string, before, after blobResult) {
	fmt.Printf("  [%s] %q (%s)\n", role, c.path, kind)
	if before.skipped != notSkipped || after.skipped != notSkipped {
		fmt.Printf("    comparison incomplete: before=%s, after=%s\n", before.skipped, after.skipped)
		return
	}
	fmt.Printf("    - %s\n    + %s\n", strings.Join(before.expressions, ", "), strings.Join(after.expressions, ", "))
	if before.spdx != after.spdx {
		fmt.Printf("    SPDX declaration: %t -> %t\n", before.spdx, after.spdx)
	}
}

func (t *historyTotals) record(c change, before, after blobResult) string {
	if t.lastCommit != c.commit {
		t.commits++
		t.lastCommit = c.commit
	}
	if before.skipped != notSkipped || after.skipped != notSkipped {
		t.incomplete++
		return "incomplete"
	}
	if !before.spdx && after.spdx {
		t.spdxAdded++
	}
	if before.spdx && !after.spdx {
		t.spdxRemoved++
	}
	switch {
	case c.oldMode == "000000":
		t.additions++
		return "file added"
	case c.newMode == "000000":
		t.deletions++
		return "file deleted"
	case !equalExprs(before.expressions, after.expressions):
		t.expressions++
		return "expressions changed"
	default:
		return "SPDX declaration changed"
	}
}

func printMonthly(months map[string]*historyTotals) error {
	w := csv.NewWriter(os.Stdout)
	if err := w.Write([]string{"month", "group", "commits", "expression_changes", "file_additions", "file_deletions", "incomplete", "spdx_added", "spdx_removed"}); err != nil {
		return err
	}
	keys := make([]string, 0, len(months))
	for key := range months {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t := months[key]
		row := strings.Split(key, ",")
		for _, n := range []int{t.commits, t.expressions, t.additions, t.deletions, t.incomplete, t.spdxAdded, t.spdxRemoved} {
			row = append(row, strconv.Itoa(n))
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
