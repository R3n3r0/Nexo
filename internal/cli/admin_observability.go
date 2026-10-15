package cli

import (
	"context"
	"errors"
	"io"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// Trace implements AdminClient.
func (c *adminClient) Trace(ctx context.Context, opts nexoadmin.TraceOptions) (EventStream[nexoadmin.TraceEvent], error) {
	s, err := c.api.Trace(ctx, opts)
	if err != nil {
		return nil, MapAdminError(err, c.endpoint)
	}
	return &adminStream[nexoadmin.TraceEvent]{stream: s, client: c}, nil
}

// Logs implements AdminClient.
func (c *adminClient) Logs(ctx context.Context, opts nexoadmin.LogOptions) (EventStream[nexoadmin.LogEntry], error) {
	s, err := c.api.Logs(ctx, opts)
	if err != nil {
		return nil, MapAdminError(err, c.endpoint)
	}
	return &adminStream[nexoadmin.LogEntry]{stream: s, client: c}, nil
}

// SearchAudit implements AdminClient.
func (c *adminClient) SearchAudit(ctx context.Context, q nexoadmin.AuditQuery) (*nexoadmin.AuditList, error) {
	v, err := c.api.SearchAudit(ctx, q)
	return mapped(c, v, err)
}

// ListMetrics implements AdminClient.
func (c *adminClient) ListMetrics(ctx context.Context) (*nexoadmin.MetricList, error) {
	v, err := c.api.ListMetrics(ctx)
	return mapped(c, v, err)
}

// QueryMetric implements AdminClient.
func (c *adminClient) QueryMetric(ctx context.Context, q nexoadmin.MetricQuery) (*nexoadmin.MetricSeries, error) {
	v, err := c.api.QueryMetric(ctx, q)
	return mapped(c, v, err)
}

// adminStream adapts a stream of pkg/nexoadmin to EventStream: io.EOF is
// kept as is (the server ended the stream), the other failures are mapped
// like those of the requests.
type adminStream[T any] struct {
	stream *nexoadmin.Stream[T]
	client *adminClient
}

var _ EventStream[nexoadmin.TraceEvent] = (*adminStream[nexoadmin.TraceEvent])(nil)

// Next implements EventStream.
func (s *adminStream[T]) Next() (*T, error) {
	v, err := s.stream.Next()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, MapAdminError(err, s.client.endpoint)
	}
	return v, err
}

// Dropped implements EventStream.
func (s *adminStream[T]) Dropped() uint64 { return s.stream.Dropped() }

// Close implements EventStream.
func (s *adminStream[T]) Close() error { return s.stream.Close() }
