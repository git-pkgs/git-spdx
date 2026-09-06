package main

var historyWorkers = newOption(1)

const historyLookahead = 2

type historyResult struct {
	changes []change
	err     error
}
