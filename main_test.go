package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("GIT_SPDX_TEST_CLI") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func repository(t *testing.T, options ...string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, append([]string{"init", "-b", "main"}, options...)...)
	git(t, repo, "config", "user.name", "Test")
	git(t, repo, "config", "user.email", "test@example.org")
	git(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

func commitFile(t *testing.T, repo, name, content, subject string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "--", name)
	git(t, repo, "commit", "-m", subject)
}

func cli(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GIT_SPDX_TEST_CLI=1", "GOMAXPROCS=2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git spdx %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestCLI(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "source.go", "// SPDX-License-Identifier: MIT\npackage example\n", "Add source")
	commitFile(t, repo, "source.go", "// SPDX-License-Identifier: Apache-2.0\npackage example\n", "Change expression")
	for _, command := range []string{"scan", "log"} {
		t.Run(command, func(t *testing.T) {
			out := cli(t, "-readers", "2", command, repo)
			for _, expression := range []string{"MIT", "Apache-2.0"} {
				if !strings.Contains(out, expression) {
					t.Fatalf("missing %s:\n%s", expression, out)
				}
			}
		})
	}
}
