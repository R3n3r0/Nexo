// Command nexoctl is the Nexo command-line client. The commands live in
// internal/cli/commands; main only wires the process (standard streams,
// environment, signals) and exits with the code of docs/06 TCL-U-04.
package main

import (
	"os"

	"github.com/r3n3r0/nexo/internal/cli/commands"
)

func main() {
	os.Exit(commands.Run(commands.DefaultDeps(), os.Args[1:]))
}
