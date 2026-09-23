package main

import githistory "github.com/git-pkgs/history"

type historySpool struct {
	spool *githistory.Spool
}

func spoolLogHistory(repo string) (*historySpool, map[string]bool, error) {
	spool, err := githistory.NewSpool("git-spdx")
	if err != nil {
		return nil, nil, err
	}
	result := &historySpool{spool: spool}
	legal := make(map[string]bool)
	err = walkChanges(repo, true, func(c change) {
		recordLegalChange(legal, c)
		if !c.merge {
			spool.Write(changeToHistory(c))
		}
	})
	if err == nil {
		err = spool.Ready()
	}
	if err != nil {
		result.close()
		return nil, nil, err
	}
	return result, legal, nil
}

func (s *historySpool) replay(visit func(change)) error {
	return s.spool.Replay(func(c githistory.Change) {
		visit(changeFromHistory(c))
	})
}

func (s *historySpool) close() {
	s.spool.Close()
}
