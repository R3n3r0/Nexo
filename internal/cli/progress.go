package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
)

// progressInterval is the minimum time between two redraws of the bar.
const progressInterval = 100 * time.Millisecond

// progressWidth is the number of cells of the bar.
const progressWidth = 30

// Progress is a single-line progress bar for the transfers of cp, mv,
// mirror and pipe, drawn on a terminal with carriage returns. A disabled
// Progress (no terminal, --quiet, --json) does nothing. It is safe for
// concurrent use: the parts of a multipart upload report from several
// goroutines.
type Progress struct {
	w       io.Writer
	enabled bool
	clock   clock.Clock

	mu       sync.Mutex
	total    int64
	done     int64
	start    time.Time
	lastDraw time.Time
	lastLen  int
	label    string
	finished bool
}

// NewProgress returns a bar over total bytes (0 or less: unknown) drawn on
// w when enabled.
func NewProgress(w io.Writer, total int64, enabled bool, clk clock.Clock) *Progress {
	if clk == nil {
		clk = clock.NewReal()
	}
	return &Progress{w: w, enabled: enabled, clock: clk, total: total, start: clk.Now()}
}

// Enabled reports whether the bar is drawn.
func (p *Progress) Enabled() bool { return p != nil && p.enabled }

// SetLabel sets the text shown before the bar (the current file).
func (p *Progress) SetLabel(label string) {
	if !p.Enabled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.label = label
	p.drawLocked(false)
}

// AddTotal raises the expected total by n bytes.
func (p *Progress) AddTotal(n int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total += n
}

// Add records n transferred bytes.
func (p *Progress) Add(n int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	if p.enabled {
		p.drawLocked(false)
	}
}

// Done returns the transferred bytes.
func (p *Progress) Done() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// Elapsed returns the time since the bar was created.
func (p *Progress) Elapsed() time.Duration {
	if p == nil {
		return 0
	}
	return p.clock.Now().Sub(p.start)
}

// Finish draws the final state and ends the line.
func (p *Progress) Finish() {
	if !p.Enabled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	p.drawLocked(true)
	_, _ = io.WriteString(p.w, "\n")
}

// Render returns the current text of the bar.
func (p *Progress) Render() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.renderLocked()
}

func (p *Progress) drawLocked(force bool) {
	if p.finished && !force {
		return
	}
	now := p.clock.Now()
	if !force && !p.lastDraw.IsZero() && now.Sub(p.lastDraw) < progressInterval {
		return
	}
	p.lastDraw = now
	line := p.renderLocked()
	pad := ""
	if n := p.lastLen - len(line); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	p.lastLen = len(line)
	_, _ = io.WriteString(p.w, "\r"+line+pad)
}

func (p *Progress) renderLocked() string {
	elapsed := p.clock.Now().Sub(p.start)
	var speed int64
	if secs := elapsed.Seconds(); secs > 0 {
		speed = int64(float64(p.done) / secs)
	}
	label := p.label
	if len(label) > 32 {
		label = "…" + label[len(label)-31:]
	}
	var b strings.Builder
	if label != "" {
		fmt.Fprintf(&b, "%-32s ", label)
	}
	if p.total > 0 {
		ratio := float64(p.done) / float64(p.total)
		ratio = min(max(ratio, 0), 1)
		filled := int(ratio * progressWidth)
		bar := strings.Repeat("=", filled)
		if filled < progressWidth {
			bar += ">" + strings.Repeat(" ", progressWidth-filled-1)
		}
		fmt.Fprintf(&b, "[%s] %3d%% %s/%s", bar, int(ratio*100), HumanSize(p.done), HumanSize(p.total))
	} else {
		fmt.Fprintf(&b, "%s", HumanSize(p.done))
	}
	fmt.Fprintf(&b, " %s/s", HumanSize(speed))
	if p.total > 0 && speed > 0 && p.done < p.total {
		eta := time.Duration(float64(p.total-p.done) / float64(speed) * float64(time.Second))
		fmt.Fprintf(&b, " ETA %s", eta.Round(time.Second))
	}
	return b.String()
}
