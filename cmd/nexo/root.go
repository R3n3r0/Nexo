package main

import (
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/version"
)

// cmdDeps are the process-level dependencies of the commands, injected so
// that the commands can be tested without touching the real environment.
type cmdDeps struct {
	stdout    io.Writer
	stderr    io.Writer
	lookupEnv config.LookupEnv
	client    *http.Client
}

func newRootCmd(deps cmdDeps) *cobra.Command {
	root := &cobra.Command{
		Use:           "nexo",
		Short:         "Nexo S3-compatible object storage server",
		Long:          "Nexo is an S3-compatible object storage server with an embedded web console.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// nexo --version prints what nexo version prints, as nexoctl
		// --version does.
		Version: version.Get().String(),
	}
	root.SetVersionTemplate("nexo {{.Version}}\n")
	root.SetOut(deps.stdout)
	root.SetErr(deps.stderr)
	root.AddCommand(newServerCmd(deps), newVersionCmd(deps), newHealthcheckCmd(deps))
	return root
}
