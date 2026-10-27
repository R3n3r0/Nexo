package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// updateRecord is the JSON record of an update or a coordinated restart.
type updateRecord struct {
	Alias string `json:"alias"`
	nexoadmin.UpdateJob
}

// newAdminUpdateCmd returns admin update (S-OPS-09).
func newAdminUpdateCmd(e *env) *cobra.Command {
	var (
		sum               string
		allAtOnce, status bool
		noWait, yes       bool
		timeout           time.Duration
	)
	cmd := &cobra.Command{
		Use:   "update [flags] ALIAS URL",
		Short: "Update the server binary of every node (rolling, without downtime)",
		Long: `Update the server binary of every node of the deployment to the binary at URL (the release
archive of nexo for the platform of the nodes, extracted: the binary itself), whose SHA-256 --sha256
gives.

Every node downloads the binary next to the one it runs, verifies its digest and runs it to read its
version; nothing changes unless every node succeeds. The nodes then restart with the new binary one at a
time: a node only when one more node can stop without losing the write quorum of any erasure set, the
next one once it is back with the new version. The node receiving the request restarts last. The
previous binary of each node is kept as "<binary>.old". --all-at-once restarts every node together (a
short outage; required to change the major version).

The binary must be in a directory the user of the server can write. In a container or on Kubernetes the
image is the binary: update the image instead (the operator rolls the pods).

The command waits for the job to end, printing the progress of the nodes; --no-wait returns at once.
--status prints the last update or coordinated restart.`,
		Example: `  nexoctl admin update --sha256 3f5a... myminio https://dl.example.com/nexo-v1.1.0-linux-amd64
  nexoctl admin update --status myminio`,
		Args: func(cmd *cobra.Command, args []string) error {
			if status {
				return exactArgs(1)(cmd, args)
			}
			return exactArgs(2)(cmd, args)
		},
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if status {
				j, err := client.UpdateStatus(ctx)
				if err != nil {
					return err
				}
				return e.out.Result(updateRecord{Alias: alias, UpdateJob: *j}, formatJob(j))
			}
			if sum == "" {
				return cli.UsageError("--sha256 is required: the SHA-256 of the binary (sha256sum, or the checksums file of the release)")
			}
			mode := "rolling"
			if allAtOnce {
				mode = "all"
			}
			if err := e.confirm(yes, fmt.Sprintf("Update every node of %q to %s (%s)?", alias, args[1], mode)); err != nil {
				return err
			}
			j, err := client.StartUpdate(ctx, nexoadmin.UpdateRequest{URL: args[1], SHA256: sum, Mode: mode})
			if err != nil {
				return fmt.Errorf("update %s: %w", alias, err)
			}
			return e.followJob(ctx, client, alias, j, noWait, timeout)
		},
	}
	f := cmd.Flags()
	f.StringVar(&sum, "sha256", "", "SHA-256 of the binary (hex)")
	f.BoolVar(&allAtOnce, "all-at-once", false, "restart every node together (short outage) instead of one at a time")
	f.BoolVar(&status, "status", false, "print the last update or coordinated restart")
	addJobFlags(cmd, &noWait, &timeout)
	f.BoolVarP(&yes, "yes", "y", false, "do not ask for a confirmation")
	return cmd
}

// addJobFlags adds the flags of the commands that start a job.
func addJobFlags(cmd *cobra.Command, noWait *bool, timeout *time.Duration) {
	cmd.Flags().BoolVar(noWait, "no-wait", false, "return once the job started")
	cmd.Flags().DurationVar(timeout, "timeout", 30*time.Minute, "how long to wait for the job")
}

// followJob waits for a job to end, printing the changes of its nodes,
// unless noWait. The node answering may restart meanwhile: the errors are
// retried until the timeout.
func (e *env) followJob(ctx context.Context, client cli.AdminClient, alias string, j *nexoadmin.UpdateJob, noWait bool, timeout time.Duration) error {
	if noWait {
		return e.out.Info(updateRecord{Alias: alias, UpdateJob: *j},
			fmt.Sprintf("Job %s (%s, %s) started on %q: nexoctl admin update --status %s", j.ID, j.Kind, j.Mode, alias, alias))
	}
	_ = e.out.Note(fmt.Sprintf("Job %s (%s, %s) started by node %s.", j.ID, j.Kind, j.Mode, j.Node))
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	seen := map[string]string{}
	var lastErr error
	for {
		if j != nil {
			for _, n := range j.Nodes {
				if seen[n.Node] != n.Phase {
					seen[n.Node] = n.Phase
					_ = e.out.Note(fmt.Sprintf("  %-28s %s", n.Node, nodeLine(n)))
				}
			}
			if j.Done() {
				break
			}
		}
		select {
		case <-ctx.Done():
			err := fmt.Errorf("the job did not end within %s (nexoctl admin update --status %s)", timeout, alias)
			if lastErr != nil {
				err = fmt.Errorf("%w: last error: %w", err, lastErr)
			}
			return err
		case <-time.After(e.deps.PollInterval):
		}
		cur, err := client.UpdateStatus(ctx)
		if err != nil {
			// The node answering restarts: try again.
			lastErr = err
			continue
		}
		j, lastErr = cur, nil
	}
	if j.State != "completed" {
		return errors.New(jobFailure(j))
	}
	rec := updateRecord{Alias: alias, UpdateJob: *j}
	done := "Every node of %q restarted."
	if j.Kind == "update" {
		done = "Every node of %q runs " + j.Version + "."
	}
	return e.out.Info(rec, fmt.Sprintf(done, alias))
}

func nodeLine(n nexoadmin.UpdateNode) string {
	s := n.Phase
	if n.Version != "" {
		s += " (" + n.Version + ")"
	}
	if n.Error != "" {
		s += ": " + n.Error
	}
	return s
}

func jobFailure(j *nexoadmin.UpdateJob) string {
	msg := fmt.Sprintf("job %s (%s) failed", j.ID, j.Kind)
	if j.Error != "" {
		msg += ": " + j.Error
	}
	return msg
}

// formatJob renders a job for --status.
func formatJob(j *nexoadmin.UpdateJob) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job %s: %s (%s) %s, started %s by %s", j.ID, j.Kind, j.Mode, j.State, j.Started.UTC().Format(time.DateTime), j.Node)
	if !j.Finished.IsZero() {
		fmt.Fprintf(&b, ", ended %s", j.Finished.UTC().Format(time.DateTime))
	}
	if j.Kind == "update" {
		fmt.Fprintf(&b, "\nBinary: %s (sha256 %s)", j.URL, j.SHA256)
		if j.Version != "" {
			fmt.Fprintf(&b, ", version %s", j.Version)
		}
	}
	if j.Error != "" {
		fmt.Fprintf(&b, "\nError: %s", j.Error)
	}
	for _, n := range j.Nodes {
		fmt.Fprintf(&b, "\n  %-28s %s", n.Node, nodeLine(n))
	}
	return b.String()
}
