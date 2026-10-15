package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// eventInstruments are the instruments of the notification decorators.
type eventInstruments struct {
	sent      Counter
	failed    Counter
	duration  Histogram
	published Counter
	dropped   Counter
}

func newEventInstruments(ctx context.Context, reg Registry) (*eventInstruments, error) {
	m := &eventInstruments{}
	var err error
	counter := func(dst *Counter, name, help string, labels ...string) {
		if err == nil {
			*dst, err = reg.Counter(ctx, Definition{Subsystem: "events", Name: name, Help: help, Labels: labels})
		}
	}
	counter(&m.sent, "sent_total", "Bucket notifications delivered, by target.", "target")
	counter(&m.failed, "failed_total", "Failed deliveries of bucket notifications (retried from the queue of the target), by target.", "target")
	counter(&m.published, "published_total", "Bucket notifications queued for routing.")
	counter(&m.dropped, "dropped_total", "Bucket notifications that could not be queued (queue full or dispatcher stopped).")
	if err == nil {
		m.duration, err = reg.Histogram(ctx, Definition{Subsystem: "events", Name: "send_duration_seconds",
			Help: "Latency of the deliveries of bucket notifications, by target.", Labels: []string{"target"}})
	}
	if err != nil {
		return nil, fmt.Errorf("metrics: events: %w", err)
	}
	return m, nil
}

// WrapTargetFactory returns f with every target it builds measured: the
// deliveries, their failures and their latency, by target ("<id>:<type>",
// bounded by the configuration).
func WrapTargetFactory(ctx context.Context, f event.TargetFactory, reg Registry) (event.TargetFactory, error) {
	if f == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the target factory decorator needs a factory and a registry", domain.ErrInvalidArgument)
	}
	m, err := newEventInstruments(ctx, reg)
	if err != nil {
		return nil, err
	}
	return &measuredFactory{next: f, m: m}, nil
}

type measuredFactory struct {
	next event.TargetFactory
	m    *eventInstruments
}

var _ event.TargetFactory = (*measuredFactory)(nil)

// New implements event.TargetFactory.
func (f *measuredFactory) New(ctx context.Context, cfg event.TargetConfig) (event.Target, error) {
	t, err := f.next.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &measuredTarget{next: t, m: f.m, id: t.ID().String()}, nil
}

type measuredTarget struct {
	next event.Target
	m    *eventInstruments
	id   string
}

var _ event.Target = (*measuredTarget)(nil)

// ID implements event.Target.
func (t *measuredTarget) ID() event.TargetID { return t.next.ID() }

// Send implements event.Target.
func (t *measuredTarget) Send(ctx context.Context, e event.Event) error {
	start := time.Now()
	err := t.next.Send(ctx, e)
	t.m.duration.Observe(ctx, time.Since(start).Seconds(), t.id)
	if err != nil {
		t.m.failed.Add(ctx, 1, t.id)
		return err
	}
	t.m.sent.Add(ctx, 1, t.id)
	return nil
}

// Close implements event.Target.
func (t *measuredTarget) Close(ctx context.Context) error { return t.next.Close(ctx) }

// WrapEvents returns d measured: the notifications published and the ones
// that could not be queued.
func WrapEvents(ctx context.Context, d event.Dispatcher, reg Registry) (event.Dispatcher, error) {
	if d == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the events decorator needs a dispatcher and a registry", domain.ErrInvalidArgument)
	}
	m, err := newEventInstruments(ctx, reg)
	if err != nil {
		return nil, err
	}
	return &measuredEvents{Dispatcher: d, m: m}, nil
}

// measuredEvents decorates Publish; the other methods are the ones of the
// dispatcher.
type measuredEvents struct {
	event.Dispatcher
	m *eventInstruments
}

var _ event.Dispatcher = (*measuredEvents)(nil)

// Publish implements event.Service.
func (d *measuredEvents) Publish(ctx context.Context, e event.Event) error {
	err := d.Dispatcher.Publish(ctx, e)
	if e.EventName.IsInternal() {
		return err
	}
	if err != nil {
		d.m.dropped.Add(ctx, 1)
		return err
	}
	d.m.published.Add(ctx, 1)
	return nil
}
