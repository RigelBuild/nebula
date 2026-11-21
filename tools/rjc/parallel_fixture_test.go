package main

import "testing"

func parallelFixtureRepo(t *testing.T) *fixture {
	t.Helper()
	return makeFixtureRepo(t, true)
}
