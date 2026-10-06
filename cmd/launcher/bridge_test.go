package main

import "testing"

func TestBridgeRoutingIgnoresOperationArguments(t *testing.T) {
	for _, args := range [][]string{{"bridge", "--protocol", "1"}, {"--root", "instance", "bridge"}, {"--root=instance", "bridge"}} {
		if !isBridgeCommand(args) {
			t.Fatalf("bridge was not routed: %q", args)
		}
	}
	for _, args := range [][]string{nil, {"push", "-m", "bridge"}, {"add", "bridge"}, {"--root", "bridge", "sync"}} {
		if isBridgeCommand(args) {
			t.Fatalf("CLI was routed as bridge: %q", args)
		}
	}
}
