package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The metadata log of the drives (docs/user/operations/metadata-log.md):
// its state on every drive of the deployment and its compaction on
// demand.

// metadataLogRecord is a drive in log mode (admin metadata-log status).
type metadataLogRecord struct {
	Alias    string `json:"alias"`
	Node     string `json:"node"`
	Endpoint string `json:"endpoint"`
	nexoadmin.MetadataLog
}

// metadataLogCompactionRecord is the compaction of a drive (admin
// metadata-log compact).
type metadataLogCompactionRecord struct {
	Alias string `json:"alias"`
	nexoadmin.MetadataLogDriveCompaction
}

func newAdminMetadataLogCmd(e *env) *cobra.Command {
	cmd := groupCmd("metadata-log", "Inspect and compact the metadata log of the drives")
	cmd.Long = `Inspect and compact the metadata log of the drives (servers started with
--metadata-log): the segments, the share of their bytes still live, the objects
indexed and the memory of the index, the group commits since the start.`
	cmd.AddCommand(newAdminMetadataLogStatusCmd(e), newAdminMetadataLogCompactCmd(e))
	return cmd
}

func newAdminMetadataLogStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "status ALIAS",
		Short:             "Show the metadata log of every drive of the deployment",
		Example:           "  nexoctl admin metadata-log status local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias, client, err := e.adminAlias(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			info, err := client.ServerInfo(cmd.Context())
			if err != nil {
				return err
			}
			found := false
			for _, n := range info.Nodes {
				for _, d := range n.Disks {
					l := d.MetadataLog
					if l == nil {
						continue
					}
					found = true
					path := d.Path
					if path == "" {
						path = d.Endpoint
					}
					// #nosec G115 -- display of byte counters, far below 2^63
					text := fmt.Sprintf("%s  %d segment(s), %s (%s live), %d object(s), index %s of %s\n  %d flush(es) of %d record(s), %d compaction(s) reclaimed %s",
						e.out.Paint(cli.ColorCyan, path), l.Segments, cli.HumanSize(l.Bytes), livePercent(l), l.Objects,
						cli.HumanSize(int64(l.IndexBytes)), cli.HumanSize(int64(l.IndexBudget)),
						l.Flushes, l.Records, l.Compactions, cli.HumanSize(int64(l.CompactedBytes)))
					if l.DamagedAreas > 0 {
						text += e.out.Paint(cli.ColorYellow, fmt.Sprintf(", %d damaged area(s) skipped", l.DamagedAreas))
					}
					if err := e.out.Result(metadataLogRecord{Alias: alias, Node: n.Endpoint, Endpoint: d.Endpoint, MetadataLog: *l}, text); err != nil {
						return err
					}
				}
			}
			if !found {
				return e.out.Note("No drive uses the metadata log (start the server with --metadata-log).")
			}
			return nil
		},
	}
}

// livePercent is the share of the bytes of a log still live.
func livePercent(l *nexoadmin.MetadataLog) string {
	if l.Bytes <= 0 {
		return "100%"
	}
	return fmt.Sprintf("%.0f%%", float64(l.LiveBytes)*100/float64(l.Bytes))
}

func newAdminMetadataLogCompactCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "compact ALIAS",
		Short: "Compact the metadata log of the drives of a node now",
		Long: `Rewrite now every segment of the metadata log holding garbage, on the drives of the
node that answers the alias (in a cluster, run it against each node). The live
records are copied and flushed before an old segment is removed: no metadata is
lost. The server also compacts on its own the segments less than half live.`,
		Example:           "  nexoctl admin metadata-log compact local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias, client, err := e.adminAlias(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			res, err := client.CompactMetadataLog(cmd.Context())
			if err != nil {
				return err
			}
			if len(res.Drives) == 0 {
				return e.out.Note("No drive of the node uses the metadata log.")
			}
			failed := 0
			for _, d := range res.Drives {
				text := fmt.Sprintf("%s  %d segment(s) rewritten, %s reclaimed", e.out.Paint(cli.ColorCyan, d.Endpoint), d.Segments, cli.HumanSize(d.ReclaimedBytes))
				if d.Error != "" {
					failed++
					text = fmt.Sprintf("%s  %s", e.out.Paint(cli.ColorCyan, d.Endpoint), e.out.Paint(cli.ColorRed, "error: "+d.Error))
				}
				if err := e.out.Result(metadataLogCompactionRecord{Alias: alias, MetadataLogDriveCompaction: d}, text); err != nil {
					return err
				}
			}
			if failed > 0 {
				return fmt.Errorf("compact %s: %d drive(s) failed", alias, failed)
			}
			return nil
		},
	}
}
