package main

import (
	"fmt"
	"io"
	"os"
)

const usage = "Usage: manage <command>\nNo management commands are implemented yet.\n"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprint(stderr, usage)
	if len(args) != 0 {
		// Do not echo arbitrary arguments: later commands may handle secrets.
		fmt.Fprintln(stderr, "Unknown command.")
	}
	return 2
}
