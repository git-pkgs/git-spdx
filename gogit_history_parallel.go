package main

import "runtime"

var historyWorkers = newOption(0)

const historyLookahead = 2

// maxAutoHistoryWorkers caps --history-workers=0. On cargo's history the walk
// stopped getting faster beyond 4-8 workers and slowed slightly at 18.
const maxAutoHistoryWorkers = 8

func resolvedHistoryWorkers() int {
	if *historyWorkers > 0 {
		return *historyWorkers
	}
	return min(runtime.GOMAXPROCS(0), maxAutoHistoryWorkers)
}

type historyResult struct {
	changes []change
	err     error
}
