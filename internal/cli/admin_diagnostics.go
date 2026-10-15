package cli

import (
	"context"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// Profile implements AdminClient.
func (c *adminClient) Profile(ctx context.Context, in nexoadmin.ProfileRequest) (*nexoadmin.Profile, error) {
	v, err := c.api.Profile(ctx, in)
	return mapped(c, v, err)
}

// Speedtest implements AdminClient.
func (c *adminClient) Speedtest(ctx context.Context, kind string, in nexoadmin.SpeedtestRequest) (*nexoadmin.SpeedtestResult, error) {
	v, err := c.api.Speedtest(ctx, kind, in)
	return mapped(c, v, err)
}
