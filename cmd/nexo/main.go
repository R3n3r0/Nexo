// Command nexo is the Nexo object storage server.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	deps := cmdDeps{
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		lookupEnv: os.LookupEnv,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
	if err := newRootCmd(deps).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
