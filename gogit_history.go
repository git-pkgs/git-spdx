package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

func walkChangesGoGit(path string, merges bool, visit func(change)) error {
	r, err := openGoGit(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return walkChangesGoGitStream(r, merges, visit)
}

func historyRoots(r *gogit.Repository) ([]plumbing.Hash, error) {
	refs, err := r.References()
	if err != nil {
		return nil, err
	}
	defer refs.Close()
	var roots []plumbing.Hash
	head, err := r.Head()
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, err
	}
	if err == nil {
		h, ok, err := commitAtReference(r, head.Hash())
		if err != nil {
			return nil, err
		}
		if ok {
			roots = append(roots, h)
		}
	}
	for {
		ref, err := refs.Next()
		if err == io.EOF {
			return roots, nil
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(ref.Name().String(), "refs/") {
			continue
		}
		if strings.HasPrefix(ref.Name().String(), "refs/replace/") {
			return nil, fmt.Errorf("gogit backend does not support replacement refs")
		}
		resolved, err := r.Reference(ref.Name(), true)
		if err != nil {
			return nil, err
		}
		h, ok, err := commitAtReference(r, resolved.Hash())
		if err != nil {
			return nil, fmt.Errorf("reference %s: %w", ref.Name(), err)
		}
		if ok {
			roots = append(roots, h)
		}
	}
}

func commitAtReference(r *gogit.Repository, h plumbing.Hash) (plumbing.Hash, bool, error) {
	seen := make(map[plumbing.Hash]bool)
	for {
		if seen[h] {
			return plumbing.ZeroHash, false, fmt.Errorf("cyclic tag at %s", h)
		}
		seen[h] = true
		o, err := r.Object(plumbing.AnyObject, h)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		switch v := o.(type) {
		case *object.Commit:
			return v.Hash, true, nil
		case *object.Tag:
			h = v.Target
		default:
			return plumbing.ZeroHash, false, nil
		}
	}
}

func commitSubject(message string) string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(message), "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	return strings.Join(lines, " ")
}

func isShallowGoGit(path string) (bool, error) {
	r, err := openGoGit(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = r.Close() }()
	shallow, err := r.Storer.Shallow()
	return len(shallow) != 0, err
}

func openGoGit(path string) (*gogit.Repository, error) {
	for _, name := range []string{"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		if os.Getenv(name) != "" {
			return nil, fmt.Errorf("gogit backend does not support %s", name)
		}
	}
	r, err := gogit.PlainOpen(path)
	if err != nil {
		return nil, err
	}
	fs := r.Storer.(*filesystem.Storage).Filesystem()
	for _, name := range []string{"objects/info/alternates", "info/grafts"} {
		f, err := fs.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			var data []byte
			data, err = io.ReadAll(f)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil && len(strings.TrimSpace(string(data))) > 0 {
				err = fmt.Errorf("gogit backend does not support %s", name)
			}
		}
		if err != nil {
			_ = r.Close()
			return nil, err
		}
	}
	if err := configureGoGit(r); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}
