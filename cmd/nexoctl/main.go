// Command nexoctl is the Nexo command-line client.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	deps := cmdDeps{stdout: os.Stdout, stderr: os.Stderr, configPath: ""}
	if err := newRootCmd(deps).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
