package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// adminProfileRecord describes a collected profile (admin profile).
type adminProfileRecord struct {
	Alias string `json:"alias"`
	Node  string `json:"node"`
	Type  string `json:"type"`
	File  string `json:"file"`
	Bytes int    `json:"bytes"`
}

// adminSpeedtestRecord is the result of a speed test (admin speedtest).
type adminSpeedtestRecord struct {
	Alias string `json:"alias"`
	nexoadmin.SpeedtestResult
}

func newAdminProfileCmd(e *env) *cobra.Command {
	var (
		kind     string
		duration time.Duration
		output   string
	)
	cmd := &cobra.Command{
		Use:   "profile [flags] ALIAS",
		Short: "Collect a runtime profile (pprof) of the server",
		Long: `Collect a runtime profile of the node that serves the Admin API of the alias, in the
pprof format: analyze it with "go tool pprof FILE". cpu, mutex and block sample for
--duration (1s to 2m); heap and goroutine are snapshots. In a cluster each node is
profiled through its own console address (or the alias of that node).

Needs admin:Profiling (root, the diagnostics policy).`,
		Example: "  nexoctl admin profile local\n  nexoctl admin profile --type heap --output heap.pb.gz local\n" +
			"  nexoctl admin profile --type mutex --duration 30s local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			switch kind {
			case nexoadmin.ProfileCPU, nexoadmin.ProfileHeap, nexoadmin.ProfileGoroutine, nexoadmin.ProfileMutex, nexoadmin.ProfileBlock:
			default:
				return cli.UsageError("--type %q must be cpu, heap, goroutine, mutex or block", kind)
			}
			if duration < 0 || duration%time.Second != 0 {
				return cli.UsageError("--duration must be a whole number of seconds")
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if kind == nexoadmin.ProfileCPU || kind == nexoadmin.ProfileMutex || kind == nexoadmin.ProfileBlock {
				shown := duration
				if shown == 0 {
					shown = 10 * time.Second
				}
				_ = e.out.Note(fmt.Sprintf("Sampling the %s profile of %q for %s...", kind, alias, shown))
			}
			p, err := client.Profile(ctx, nexoadmin.ProfileRequest{Type: kind, Seconds: int(duration / time.Second)})
			if err != nil {
				return fmt.Errorf("profile %s: %w", alias, err)
			}
			file := output
			if file == "" {
				file = p.FileName
			}
			if file == "" {
				file = "nexo-" + kind + ".pb.gz"
			}
			// The name suggested by the server never leaves the current
			// directory.
			if output == "" {
				file = filepath.Base(file)
			}
			if err := os.WriteFile(file, p.Data, 0o600); err != nil {
				return err
			}
			return e.out.Info(adminProfileRecord{Alias: alias, Node: p.Node, Type: kind, File: file, Bytes: len(p.Data)},
				fmt.Sprintf("Wrote the %s profile of %s (%s) to %s: go tool pprof %s", kind, p.Node, cli.HumanSize(int64(len(p.Data))), file, file))
		},
	}
	cmd.Flags().StringVarP(&kind, "type", "t", nexoadmin.ProfileCPU, "profile: cpu, heap, goroutine, mutex or block")
	cmd.Flags().DurationVarP(&duration, "duration", "d", 0, "sampling of the cpu, mutex and block profiles (default 10s, at most 2m)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "file of the profile (default: the name suggested by the server, nexo-<node>-<type>.pb.gz)")
	return cmd
}

func newAdminSpeedtestCmd(e *env) *cobra.Command {
	var (
		size        string
		duration    time.Duration
		concurrency int
	)
	cmd := &cobra.Command{
		Use:   "speedtest [flags] ALIAS drive|object|net",
		Short: "Measure the drives, the object layer or the network between the nodes",
		Long: `Run a speed test on the node that serves the Admin API of the alias and print the
throughput of each phase (write, then read), the operations per second and the latencies:

  drive   files written then read on every local drive (its temporary area)
  object  objects uploaded then downloaded through the object layer (erasure coding,
          quorum, internode RPC) in a temporary bucket nexo-speedtest-*, removed afterwards
  net     files sent to a drive of every other node of a cluster, then read back (the
          network between the nodes; not available out of a cluster)

Each phase lasts --duration (at most 1m) and writes at most 1 GiB per target. Needs
admin:Speedtest (root, the diagnostics policy). The test loads the server: run it
outside of the peak hours.`,
		Example:           "  nexoctl admin speedtest local drive\n  nexoctl admin speedtest --size 16MiB --duration 10s --concurrency 32 local object",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			kind := strings.ToLower(args[1])
			switch kind {
			case nexoadmin.SpeedtestDrive, nexoadmin.SpeedtestObject, nexoadmin.SpeedtestNet:
			default:
				return cli.UsageError("speed test %q must be drive, object or net", args[1])
			}
			var bytes int64
			if size != "" {
				n, err := cli.ParseSize(size)
				if err != nil || n <= 0 {
					return cli.UsageError("--size %q is not a size (e.g. 4MiB)", size)
				}
				bytes = n
			}
			if duration < 0 || concurrency < 0 {
				return cli.UsageError("--duration and --concurrency must not be negative")
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			_ = e.out.Note(fmt.Sprintf("Running the %s speed test of %q...", kind, alias))
			res, err := client.Speedtest(ctx, kind, nexoadmin.SpeedtestRequest{Size: bytes, DurationSeconds: duration.Seconds(), Concurrency: concurrency})
			if err != nil {
				return fmt.Errorf("speedtest %s: %w", alias, err)
			}
			return e.out.Result(adminSpeedtestRecord{Alias: alias, SpeedtestResult: *res}, speedtestTable(res))
		},
	}
	cmd.Flags().StringVar(&size, "size", "", "size of each file or object (default 4MiB, at most 64MiB)")
	cmd.Flags().DurationVar(&duration, "duration", 0, "duration of each phase (default 5s, at most 1m)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "concurrency of the object test and of the reads of the net test (default 8, at most 64)")
	return cmd
}

// speedtestTable formats the result of a speed test.
func speedtestTable(res *nexoadmin.SpeedtestResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Node %s, %s %s, %s per phase, concurrency %d\n", res.Node, res.Kind, cli.HumanSize(res.Size),
		time.Duration(res.DurationSeconds*float64(time.Second)).Round(time.Millisecond), res.Concurrency)
	fmt.Fprintf(&b, "%-40s %-6s %12s %10s %10s %10s", "TARGET", "PHASE", "THROUGHPUT", "OPS/S", "P50", "P99")
	for _, t := range res.Targets {
		for _, phase := range []struct {
			name string
			m    nexoadmin.SpeedtestMeasure
		}{{"write", t.Write}, {"read", t.Read}} {
			fmt.Fprintf(&b, "\n%-40s %-6s %10s/s %10.1f %8.1fms %8.1fms", t.Target, phase.name,
				cli.HumanSize(int64(phase.m.BytesPerSecond)), phase.m.OperationsPerSecond, phase.m.LatencyP50Ms, phase.m.LatencyP99Ms)
		}
		if t.Error != "" {
			fmt.Fprintf(&b, "\n%-40s error: %s", t.Target, t.Error)
		}
	}
	return b.String()
}
