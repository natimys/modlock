package main

import (
	"reflect"
	"testing"
)

func TestRootOptionPreservesOperationArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--root", `C:\Сборки с пробелами\minecraft`, "push", "-m", "release"},
		{"push", "-m", "release", "--root=" + `C:\Сборки с пробелами\minecraft`},
	} {
		out, root, err := splitRoot(args)
		if err != nil || root != `C:\Сборки с пробелами\minecraft` || !reflect.DeepEqual(out, []string{"push", "-m", "release"}) {
			t.Fatalf("splitRoot(%q) = %q, %q, %v", args, out, root, err)
		}
	}
}

func TestInvalidRootArguments(t *testing.T) {
	for _, args := range [][]string{{"--root"}, {"--root="}, {"--root", "--protocol"}, {"--root", "a", "--root=b"}} {
		if _, _, err := splitRoot(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestRootOptionDoesNotConsumeLiteralArguments(t *testing.T) {
	args := []string{"push", "--", "--root=a"}
	out, root, err := splitRoot(args)
	if err != nil || root != "" || !reflect.DeepEqual(out, args) {
		t.Fatalf("literal arguments changed: %q, %q, %v", out, root, err)
	}
}

func TestRootOptionDoesNotConsumeCommitMessage(t *testing.T) {
	args := []string{"push", "-m", "--root=literal", "--root", "instance"}
	out, root, err := splitRoot(args)
	if err != nil || root != "instance" || !reflect.DeepEqual(out, args[:3]) {
		t.Fatalf("commit message changed: %q, %q, %v", out, root, err)
	}
}
