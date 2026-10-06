package main

import "testing"

func TestReadyFileFailureStopsStartupBeforeAnyOperation(t *testing.T) {
	readyPath := t.TempDir() // A directory cannot be replaced by the ready file.
	operations := 0
	err := readyGate(readyPath, "1.2.3", func() { operations++ })
	if err == nil {
		t.Fatal("readyGate succeeded when ready file could not be written")
	}
	if operations != 0 {
		t.Fatalf("startup operations began %d times after ready failure", operations)
	}
}

func TestReadyGateKeepsDirectLaunchBehaviorWithoutReadyPath(t *testing.T) {
	operations := 0
	if err := readyGate("", "1.2.3", func() { operations++ }); err != nil {
		t.Fatal(err)
	}
	if operations != 1 {
		t.Fatalf("direct launch callback count = %d", operations)
	}
}
