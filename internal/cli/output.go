package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Color is an ANSI color of the human-readable output.
type Color string

// Colors of the human-readable output.
const (
	ColorBold   Color = "1"
	ColorRed    Color = "31"
	ColorGreen  Color = "32"
	ColorYellow Color = "33"
	ColorBlue   Color = "34"
	ColorCyan   Color = "36"
)

// TimeLayout is the layout of the dates of the human-readable output.
const TimeLayout = "2006-01-02 15:04:05 MST"

// PrinterOptions select the output format.
type PrinterOptions struct {
	// JSON prints JSON Lines (--json): one object per line whose first
	// member is "status" ("success" or "error").
	JSON bool
	// Quiet hides the confirmations of the actions (--quiet); results and
	// errors are still printed.
	Quiet bool
	// Color enables ANSI colors (a terminal without --no-color/NO_COLOR).
	Color bool
}

// Printer writes the results of a command on stdout and its diagnostics on
// stderr, as human-readable lines or JSON Lines. It is safe for concurrent
// use.
type Printer struct {
	out  io.Writer
	err  io.Writer
	opts PrinterOptions
	mu   sync.Mutex
}

// NewPrinter returns a Printer over stdout and stderr.
func NewPrinter(stdout, stderr io.Writer, opts PrinterOptions) *Printer {
	if opts.JSON {
		opts.Color = false
	}
	return &Printer{out: stdout, err: stderr, opts: opts}
}

// JSON reports whether the output is JSON Lines.
func (p *Printer) JSON() bool { return p.opts.JSON }

// Quiet reports whether the confirmations are hidden.
func (p *Printer) Quiet() bool { return p.opts.Quiet }

// Stdout returns the standard output (for streamed content such as cat).
func (p *Printer) Stdout() io.Writer { return p.out }

// Stderr returns the standard error.
func (p *Printer) Stderr() io.Writer { return p.err }

// Result prints a result of the command: rec as a JSON line, or text (a
// line, the newline is added) in human-readable mode. Results are never
// hidden by --quiet.
func (p *Printer) Result(rec any, text string) error {
	if p.opts.JSON {
		return p.JSONLine(p.out, "success", rec)
	}
	return p.line(p.out, text)
}

// Info prints the confirmation of an action: rec as a JSON line, or text
// unless --quiet.
func (p *Printer) Info(rec any, text string) error {
	if p.opts.JSON {
		return p.JSONLine(p.out, "success", rec)
	}
	if p.opts.Quiet {
		return nil
	}
	return p.line(p.out, text)
}

// Note prints a human-readable remark (a summary, a hint): nothing in JSON
// mode or with --quiet.
func (p *Printer) Note(text string) error {
	if p.opts.JSON || p.opts.Quiet {
		return nil
	}
	return p.line(p.out, text)
}

// Text prints human-readable text that has no JSON form (help, examples);
// nothing in JSON mode.
func (p *Printer) Text(text string) error {
	if p.opts.JSON {
		return nil
	}
	return p.line(p.out, text)
}

// Warn prints a warning on stderr (a JSON line with status "warning").
func (p *Printer) Warn(text string) {
	if p.opts.JSON {
		_ = p.JSONLine(p.err, "warning", struct {
			Message string `json:"message"`
		}{text})
		return
	}
	_ = p.line(p.err, p.Paint(ColorYellow, "warning: ")+text)
}

// ErrorRecord is the JSON form of an error.
type ErrorRecord struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	ExitCode int    `json:"exitCode"`
	// Details locate the errors of a rejected document (a malformed
	// policy: line, column, statement).
	Details []ErrorDetail `json:"details,omitempty"`
}

// Error prints a failure on stderr: {"status":"error","error":{...}} in
// JSON mode, "nexoctl: error: ..." otherwise.
func (p *Printer) Error(err error) {
	if err == nil {
		return
	}
	msg := ErrorMessage(err)
	if p.opts.JSON {
		_ = p.JSONLine(p.err, "error", struct {
			Error ErrorRecord `json:"error"`
		}{ErrorRecord{Code: ErrorCode(err), Message: msg, ExitCode: ExitCode(err), Details: ErrorDetails(err)}})
		return
	}
	_ = p.line(p.err, p.Paint(ColorRed, "nexoctl: error:")+" "+msg)
}

// ErrorMessage renders an error for the user: an interruption is
// "interrupted" (with the abort of a multipart upload, when any).
func ErrorMessage(err error) string {
	var aborted *AbortedUploadError
	if errors.As(err, &aborted) && ExitCode(err) == ExitInterrupted {
		if aborted.AbortErr != nil {
			return fmt.Sprintf("interrupted: the multipart upload %s of %s/%s could not be aborted: %v", aborted.UploadID, aborted.Bucket, aborted.Key, aborted.AbortErr)
		}
		return fmt.Sprintf("interrupted: the multipart upload %s of %s/%s was aborted", aborted.UploadID, aborted.Bucket, aborted.Key)
	}
	if ExitCode(err) == ExitInterrupted {
		return "interrupted"
	}
	return err.Error()
}

// Paint colors s when colors are enabled.
func (p *Printer) Paint(c Color, s string) string {
	if !p.opts.Color || s == "" {
		return s
	}
	return "\x1b[" + string(c) + "m" + s + "\x1b[0m"
}

// JSONLine writes rec as one JSON line on w, with "status" as the first
// member. rec must encode as a JSON object.
func (p *Printer) JSONLine(w io.Writer, status string, rec any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}
	body := bytes.TrimSpace(buf.Bytes())
	if len(body) < 2 || body[0] != '{' {
		return fmt.Errorf("encode JSON output: %T is not an object", rec)
	}
	var line bytes.Buffer
	line.WriteString(`{"status":`)
	statusJSON, _ := json.Marshal(status)
	line.Write(statusJSON)
	if len(body) > 2 {
		line.WriteByte(',')
		line.Write(body[1:])
	} else {
		line.WriteByte('}')
	}
	line.WriteByte('\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := w.Write(line.Bytes())
	return err
}

func (p *Printer) line(w io.Writer, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err := io.WriteString(w, text)
	return err
}

// FormatTime renders a date of the human-readable output, in UTC.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return strings.Repeat(" ", len("2006-01-02 15:04:05 UTC"))
	}
	return t.UTC().Format(TimeLayout)
}

// IsTerminal reports whether w is a terminal (a character device).
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
