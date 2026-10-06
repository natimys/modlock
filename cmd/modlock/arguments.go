package main

import (
	"fmt"
	"strings"
)

// splitRoot removes the global instance option without interpreting operation
// arguments. A literal -- ends option processing as usual.
func splitRoot(args []string) ([]string, string, error) {
	out := make([]string, 0, len(args))
	root := ""
	seen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}
		if arg == "-m" && i+1 < len(args) {
			out = append(out, arg, args[i+1])
			i++
			continue
		}
		if arg != "--root" && !strings.HasPrefix(arg, "--root=") {
			out = append(out, arg)
			continue
		}
		if seen {
			return nil, "", fmt.Errorf("--root must be specified only once")
		}
		seen = true
		if arg == "--root" {
			i++
			if i == len(args) || strings.HasPrefix(args[i], "--") {
				return nil, "", fmt.Errorf("--root requires a directory")
			}
			root = args[i]
		} else {
			root = strings.TrimPrefix(arg, "--root=")
		}
		if strings.TrimSpace(root) == "" {
			return nil, "", fmt.Errorf("--root requires a directory")
		}
	}
	return out, root, nil
}
