package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

type sample struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// jsonLines decodes JSON Lines and checks that "status" comes first.
func jsonLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		require.True(t, strings.HasPrefix(line, `{"status":`), "status first: %s", line)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

// covers: TCL-U-03
func TestPrinterJSONLines(t *testing.T) {
	var out, errOut bytes.Buffer
	p := cli.NewPrinter(&out, &errOut, cli.PrinterOptions{JSON: true, Quiet: true, Color: true})
	assert.True(t, p.JSON())
	require.NoError(t, p.Result(sample{Name: "a", URL: "http://x/?a=1&b=<2>"}, "ignored"))
	require.NoError(t, p.Info(sample{Name: "b"}, "ignored"))
	require.NoError(t, p.Info(struct{}{}, "ignored"))
	require.NoError(t, p.Note("not in JSON"))
	require.NoError(t, p.Text("not in JSON"))
	assert.Error(t, p.Result([]int{1}, ""), "records are objects")
	p.Warn("careful")
	p.Error(domain.ErrBucketNotFound)

	lines := jsonLines(t, out.String())
	require.Len(t, lines, 3)
	assert.Equal(t, "success", lines[0]["status"])
	assert.Equal(t, "http://x/?a=1&b=<2>", lines[0]["url"])
	assert.Contains(t, out.String(), "&b=<2>", "no HTML escaping")
	assert.Equal(t, map[string]any{"status": "success"}, lines[2])

	errs := jsonLines(t, errOut.String())
	require.Len(t, errs, 2)
	assert.Equal(t, "warning", errs[0]["status"])
	assert.Equal(t, "error", errs[1]["status"])
	assert.Equal(t, map[string]any{"code": "NotFound", "message": "bucket not found", "exitCode": float64(3)}, errs[1]["error"])
}

// covers: TCL-U-03
func TestPrinterText(t *testing.T) {
	var out, errOut bytes.Buffer
	p := cli.NewPrinter(&out, &errOut, cli.PrinterOptions{})
	require.NoError(t, p.Result(nil, "result"))
	require.NoError(t, p.Info(nil, "info"))
	require.NoError(t, p.Note("note\n"))
	require.NoError(t, p.Text("text"))
	p.Warn("careful")
	p.Error(errors.New("boom"))
	assert.Equal(t, "result\ninfo\nnote\ntext\n", out.String())
	assert.Equal(t, "warning: careful\nnexoctl: error: boom\n", errOut.String())
	assert.Equal(t, "x", p.Paint(cli.ColorRed, "x"), "no colors")

	out.Reset()
	quiet := cli.NewPrinter(&out, &errOut, cli.PrinterOptions{Quiet: true})
	require.NoError(t, quiet.Result(nil, "result"))
	require.NoError(t, quiet.Info(nil, "info"))
	require.NoError(t, quiet.Note("note"))
	assert.Equal(t, "result\n", out.String(), "--quiet keeps the results only")

	color := cli.NewPrinter(&out, &errOut, cli.PrinterOptions{Color: true})
	assert.Equal(t, "\x1b[31mx\x1b[0m", color.Paint(cli.ColorRed, "x"))
	assert.Equal(t, "", color.Paint(cli.ColorRed, ""))
	assert.Equal(t, "2026-01-02 03:04:05 UTC", cli.FormatTime(time.Date(2026, 1, 2, 4, 4, 5, 0, time.FixedZone("CET", 3600))))
	assert.Len(t, cli.FormatTime(time.Time{}), len("2026-01-02 03:04:05 UTC"))
	assert.False(t, cli.IsTerminal(&out))
}

func TestProgress(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var w bytes.Buffer
	p := cli.NewProgress(&w, 100*mib, true, clk)
	assert.True(t, p.Enabled())
	p.SetLabel("big.bin")
	clk.Advance(2 * time.Second)
	p.Add(50 * mib)
	assert.Equal(t, 50*mib, p.Done())
	assert.Equal(t, 2*time.Second, p.Elapsed())
	r := p.Render()
	assert.Contains(t, r, "big.bin")
	assert.Contains(t, r, "[===============>              ]  50% 50.0MiB/100.0MiB 25.0MiB/s ETA 2s")
	p.Add(10) // throttled: no redraw within 100ms
	drawn := w.Len()
	p.Add(10)
	assert.Equal(t, drawn, w.Len())
	clk.Advance(time.Second)
	p.AddTotal(0)
	p.Finish()
	p.Finish()
	assert.True(t, strings.HasSuffix(w.String(), "\n"))
	assert.True(t, strings.HasPrefix(w.String(), "\r"))

	unknown := cli.NewProgress(&w, 0, true, clk)
	unknown.Add(3 * kib)
	assert.Contains(t, unknown.Render(), "3.0KiB")
	assert.NotContains(t, unknown.Render(), "%")

	var none bytes.Buffer
	disabled := cli.NewProgress(&none, 10, false, nil)
	disabled.SetLabel("x")
	disabled.Add(5)
	disabled.Finish()
	assert.Equal(t, int64(5), disabled.Done())
	assert.Zero(t, none.Len())
	var nilBar *cli.Progress
	assert.False(t, nilBar.Enabled())
	nilBar.Add(1)
	nilBar.AddTotal(1)
	assert.Zero(t, nilBar.Done())
}
