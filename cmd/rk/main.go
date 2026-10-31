package main

import (
	"fmt"
	"os"
	"path/filepath"
	"rat-kingdom/internal/kingdom"
)

func main() {
	var err error
	switch filepath.Base(os.Args[0]) {
	case "git":
		// Guard directories made by an older rk link git here; rk no longer wraps Git.
		err = fmt.Errorf("this session's PATH holds a stale git guard (%s); restart the session", os.Args[0])
	case "br":
		err = kingdom.GuardedBeads(os.Args[1:])
	default:
		err = kingdom.Main(os.Args[1:], os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rk:", err)
		if len(os.Args) > 1 && os.Args[1] == "_check-command" {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
