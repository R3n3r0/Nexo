package metrics

import (
	"context"
	"errors"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/scanner"
)

// WrapLifecycle returns the lifecycle hook of the scanner measured: the
// versions expired and transitioned (nexo_ilm_actions_total{action}), the
// multipart uploads aborted and the failures of the hook.
func WrapLifecycle(ctx context.Context, l scanner.Lifecycle, reg Registry) (scanner.Lifecycle, error) {
	if l == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the lifecycle decorator needs the hook and a registry", domain.ErrInvalidArgument)
	}
	m := &measuredLifecycle{next: l}
	var err error
	if m.actions, err = reg.Counter(ctx, Definition{Subsystem: "ilm", Name: "actions_total",
		Help: "Versions the lifecycle rules acted on, by action (expire, transition).", Labels: []string{"action"}}); err != nil {
		return nil, fmt.Errorf("metrics: ilm: %w", err)
	}
	if m.aborted, err = reg.Counter(ctx, Definition{Subsystem: "ilm", Name: "uploads_aborted_total",
		Help: "Incomplete multipart uploads the lifecycle rules aborted."}); err != nil {
		return nil, fmt.Errorf("metrics: ilm: %w", err)
	}
	if m.errors, err = reg.Counter(ctx, Definition{Subsystem: "ilm", Name: "errors_total",
		Help: "Failures of the lifecycle rules on a version or the uploads of a bucket."}); err != nil {
		return nil, fmt.Errorf("metrics: ilm: %w", err)
	}
	if m.evaluated, err = reg.Counter(ctx, Definition{Subsystem: "ilm", Name: "versions_evaluated_total",
		Help: "Versions the lifecycle rules were evaluated on by the scanner."}); err != nil {
		return nil, fmt.Errorf("metrics: ilm: %w", err)
	}
	return m, nil
}

type measuredLifecycle struct {
	next      scanner.Lifecycle
	actions   Counter
	aborted   Counter
	errors    Counter
	evaluated Counter
}

var _ scanner.Lifecycle = (*measuredLifecycle)(nil)

// Apply implements scanner.Lifecycle.
func (l *measuredLifecycle) Apply(ctx context.Context, obj domain.ObjectInfo) (scanner.LifecycleAction, error) {
	a, err := l.next.Apply(ctx, obj)
	l.evaluated.Add(ctx, 1)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			l.errors.Add(ctx, 1)
		}
		return a, err
	}
	switch a {
	case scanner.LifecycleDeleted:
		l.actions.Add(ctx, 1, "expire")
	case scanner.LifecycleTransitioned:
		l.actions.Add(ctx, 1, "transition")
	}
	return a, nil
}

// AbortUploads implements scanner.Lifecycle.
func (l *measuredLifecycle) AbortUploads(ctx context.Context, bucket string) (int, error) {
	n, err := l.next.AbortUploads(ctx, bucket)
	if n > 0 {
		l.aborted.Add(ctx, float64(n))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		l.errors.Add(ctx, 1)
	}
	return n, err
}
