package main

import "flag"

var historyWorkers = flag.Int("history-workers", 1, "go-git history workers")

const historyLookahead = 2

type historyResult struct {
	changes []change
	err     error
}
