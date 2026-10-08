// Command nexoctl is the Nexo command-line client. The commands live in
// internal/cli/commands, whose Deps are the injection point of the process
// dependencies (docs/12 §12.4); main only wires the real process (standard
// streams, environment, signals) and exits with the code of docs/06
// TCL-U-04.
package main

import (
	"io"
	"os"

	"github.com/r3n3r0/nexo/internal/cli/commands"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes nexoctl with the dependencies of the real process and the
// given output streams, and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	deps := commands.DefaultDeps()
	deps.Stdout, deps.Stderr = stdout, stderr
	return commands.Run(deps, args)
}
