package main

import githistory "github.com/git-pkgs/history"

func walkChangesGoGit(path string, merges bool, visit func(change)) error {
	r, err := openHistory(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return r.WalkChanges(githistory.ChangeOptions{
		Merges:  merges,
		Workers: *historyWorkers,
	}, func(c githistory.Change) error {
		visit(changeFromHistory(c))
		return nil
	})
}

func isShallowGoGit(path string) (bool, error) {
	r, err := openHistory(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = r.Close() }()
	return r.Shallow()
}

func changeFromHistory(c githistory.Change) change {
	return change{
		commit: c.Commit, date: c.Date, subject: c.Subject,
		oldOID: c.OldOID, newOID: c.NewOID, path: c.Path,
		oldMode: c.OldMode, newMode: c.NewMode,
		merge: c.Merge,
	}
}

func changeToHistory(c change) githistory.Change {
	return githistory.Change{
		Commit: c.commit, Date: c.date, Subject: c.subject,
		OldOID: c.oldOID, NewOID: c.newOID, Path: c.path,
		OldMode: c.oldMode, NewMode: c.newMode,
		Merge: c.merge,
	}
}
