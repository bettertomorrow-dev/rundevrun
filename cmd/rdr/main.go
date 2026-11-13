// rdr manages development processes described by dev.toml.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 1 || (len(os.Args) == 2 && (os.Args[1] == "--help" || os.Args[1] == "help")) {
		fmt.Println("rdr: development process runner\nImplementation in progress. See docs/IMPLEMENTATION_PLAN.md.")
		return
	}
	fmt.Fprintln(os.Stderr, "rdr: command not implemented; see --help")
	os.Exit(2)
}
