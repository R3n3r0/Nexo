//go:build nexotest

package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
)

// Test hooks of the multi-process cluster tests (test/cluster builds
// bin/nexo with -tags nexotest; a release build ignores these variables):
const (
	// envTestClockOffset shifts the clock of the node by a duration
	// ("5s", "-3s"): clock skew between the nodes (docs/06 TC-13).
	envTestClockOffset = "NEXO_TEST_CLOCK_OFFSET"
	// envTestDialMap routes the internode connections of the node:
	// "peer=via,peer=via" dials via (a proxy of the test) instead of peer
	// (host:port), to partition the nodes or slow a link (TC-07, TC-08).
	envTestDialMap = "NEXO_TEST_DIAL_MAP"
)

// testOptions returns the application options of the test hooks set in
// the environment.
func testOptions(lookupEnv config.LookupEnv) ([]app.Option, error) {
	var opts []app.Option
	if v, ok := lookupEnv(envTestClockOffset); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envTestClockOffset, err)
		}
		opts = append(opts, app.WithClock(offsetClock{offset: d}))
	}
	if v, ok := lookupEnv(envTestDialMap); ok && v != "" {
		routes := map[string]string{}
		for _, pair := range strings.Split(v, ",") {
			peer, via, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok || peer == "" || via == "" {
				return nil, fmt.Errorf("%s: %q is not peer=via", envTestDialMap, pair)
			}
			routes[peer] = via
		}
		var d net.Dialer
		opts = append(opts, app.WithInternodeDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
			if via, ok := routes[addr]; ok {
				addr = via
			}
			return d.DialContext(ctx, network, addr)
		}))
	}
	return opts, nil
}

// offsetClock is the wall clock shifted by an offset.
type offsetClock struct{ offset time.Duration }

var _ clock.Clock = offsetClock{}

// Now implements clock.Clock.
func (c offsetClock) Now() time.Time { return time.Now().Add(c.offset) }
