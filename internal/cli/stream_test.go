package cli_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// streamOf returns a mock stream that yields items, then fails with end.
func streamOf(t *testing.T, end error, dropped uint64, items ...int) *climock.EventStream[int] {
	t.Helper()
	s := climock.NewEventStream[int](t)
	i := 0
	s.EXPECT().Next().RunAndReturn(func() (*int, error) {
		if i < len(items) {
			v := items[i]
			i++
			return &v, nil
		}
		return nil, end
	}).Maybe()
	s.EXPECT().Dropped().Return(dropped).Maybe()
	s.EXPECT().Close().Return(nil).Once()
	return s
}

func adminErr(status int) error {
	return cli.MapAdminError(&nexoadmin.APIError{Status: status, Code: "x", Detail: http.StatusText(status)}, "http://admin.test")
}

// covers: C-ADM-08, TD-U-12
func TestFollow(t *testing.T) {
	tests := []struct {
		name string
		// opens are the results of the successive opens: a stream or an
		// error.
		opens      []func(t *testing.T) (cli.EventStream[int], error)
		max        int
		wantItems  []int
		wantErr    error
		wantStatus int
		reconnects int
	}{
		{
			name: "stops after max items",
			opens: []func(t *testing.T) (cli.EventStream[int], error){
				func(t *testing.T) (cli.EventStream[int], error) { return streamOf(t, io.EOF, 0, 1, 2, 3), nil },
			},
			max: 2, wantItems: []int{1, 2},
		},
		{
			name: "reopens a stream the server ended and after a transient open failure",
			opens: []func(t *testing.T) (cli.EventStream[int], error){
				func(t *testing.T) (cli.EventStream[int], error) { return streamOf(t, io.EOF, 0, 1), nil },
				func(*testing.T) (cli.EventStream[int], error) { return nil, adminErr(http.StatusBadGateway) },
				func(t *testing.T) (cli.EventStream[int], error) {
					return streamOf(t, &url.Error{Op: "Get", URL: "x", Err: io.ErrUnexpectedEOF}, 0, 2), nil
				},
				func(t *testing.T) (cli.EventStream[int], error) { return streamOf(t, io.EOF, 0, 3), nil },
			},
			max: 3, wantItems: []int{1, 2, 3}, reconnects: 3,
		},
		{
			name: "the failure of the first open is returned",
			opens: []func(t *testing.T) (cli.EventStream[int], error){
				func(*testing.T) (cli.EventStream[int], error) { return nil, adminErr(http.StatusServiceUnavailable) },
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "a permanent failure after a reconnection is returned",
			opens: []func(t *testing.T) (cli.EventStream[int], error){
				func(t *testing.T) (cli.EventStream[int], error) { return streamOf(t, io.EOF, 0, 1), nil },
				func(*testing.T) (cli.EventStream[int], error) { return nil, adminErr(http.StatusForbidden) },
			},
			wantItems: []int{1}, wantStatus: http.StatusForbidden, reconnects: 1,
		},
		{
			name: "a permanent read failure is returned",
			opens: []func(t *testing.T) (cli.EventStream[int], error){
				func(t *testing.T) (cli.EventStream[int], error) { return streamOf(t, errors.New("decode"), 0, 1), nil },
			},
			wantItems: []int{1}, wantErr: errors.New("decode"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []int
			opened, reconnects := 0, 0
			err := cli.Follow(context.Background(), func(context.Context) (cli.EventStream[int], error) {
				require.Less(t, opened, len(tc.opens), "unexpected open")
				open := tc.opens[opened]
				opened++
				return open(t)
			}, func(v *int) error {
				got = append(got, *v)
				return nil
			}, cli.FollowOptions{ReconnectDelay: time.Millisecond, Max: tc.max, OnReconnect: func(error, time.Duration) { reconnects++ }})
			switch {
			case tc.wantStatus != 0:
				var adminErr *cli.AdminError
				require.ErrorAs(t, err, &adminErr)
				assert.Equal(t, tc.wantStatus, adminErr.API.Status)
			case tc.wantErr != nil:
				require.EqualError(t, err, tc.wantErr.Error())
			default:
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantItems, got)
			assert.Equal(t, tc.reconnects, reconnects)
		})
	}
}

// covers: C-ADM-08, TD-U-12
func TestFollowDelaysAndStops(t *testing.T) {
	// The delay doubles after each failed reopen and is reset by a
	// successful one.
	var delays []time.Duration
	opens := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := cli.Follow(ctx, func(context.Context) (cli.EventStream[int], error) {
		opens++
		switch opens {
		case 1, 4:
			return streamOf(t, io.EOF, 0), nil
		case 5:
			cancel()
			return nil, context.Canceled
		default:
			return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
		}
	}, func(*int) error { return nil }, cli.FollowOptions{ReconnectDelay: time.Millisecond,
		OnReconnect: func(_ error, d time.Duration) { delays = append(delays, d) }})
	require.NoError(t, err, "a canceled context ends the stream without error")
	assert.Equal(t, []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, time.Millisecond}, delays)

	// The dropped items are reported as they grow; an error of each stops.
	var dropped []uint64
	stop := errors.New("stop")
	n := 0
	err = cli.Follow(context.Background(), func(context.Context) (cli.EventStream[int], error) {
		return streamOf(t, io.EOF, 5, 1, 2), nil
	}, func(*int) error {
		n++
		if n == 2 {
			return stop
		}
		return nil
	}, cli.FollowOptions{OnDropped: func(d uint64) { dropped = append(dropped, d) }})
	require.ErrorIs(t, err, stop)
	assert.Equal(t, []uint64{5}, dropped)
}

// covers: C-ADM-08
func TestIsTransientStreamError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{io.EOF, true},
		{fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{adminErr(http.StatusInternalServerError), true},
		{adminErr(http.StatusTooManyRequests), true},
		{adminErr(http.StatusForbidden), false},
		{adminErr(http.StatusBadRequest), false},
		{&net.OpError{Op: "read", Err: errors.New("reset")}, true},
		{&url.Error{Op: "Get", URL: "x", Err: errors.New("refused")}, true},
		{errors.New("decode"), false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, cli.IsTransientStreamError(tc.err), "%v", tc.err)
	}
}

// covers: C-ADM-12
func TestPrometheusScrape(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name     string
		endpoint string
		kind     string
		job      string
		token    string
		want     string
		wantErr  bool
	}{
		{
			name: "cluster with a token", endpoint: "https://s3.example.com:9000", kind: cli.PrometheusCluster, token: "tok",
			want: "scrape_configs:\n  - job_name: \"nexo-cluster\"\n    metrics_path: \"/nexo/metrics/v1/cluster\"\n    scheme: https\n" +
				"    authorization:\n      type: Bearer\n      credentials: \"tok\"\n    static_configs:\n      - targets: [\"s3.example.com:9000\"]\n",
		},
		{
			name: "bucket behind a path prefix, public", endpoint: "http://proxy.local/nexo/", kind: cli.PrometheusBucket, job: "b",
			want: "scrape_configs:\n  - job_name: \"b\"\n    metrics_path: \"/nexo/nexo/metrics/v1/bucket\"\n    scheme: http\n" +
				"    scrape_interval: 1m\n    static_configs:\n      - targets: [\"proxy.local\"]\n",
		},
		{name: "unknown kind", endpoint: "http://h", kind: "drives", wantErr: true},
		{name: "not an http URL", endpoint: "ftp://h", kind: cli.PrometheusNode, wantErr: true},
		{name: "no host", endpoint: "http://", kind: cli.PrometheusNode, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := cli.NewPrometheusScrape(tc.endpoint, tc.kind, tc.job, tc.token)
			if tc.wantErr {
				require.ErrorIs(t, err, cli.ErrUsage)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, s.YAML())
		})
	}

	token, err := cli.SignPrometheusToken("AK", "SK", now, now.Add(time.Hour))
	require.NoError(t, err)
	claims := &jwt.RegisteredClaims{}
	_, err = jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return []byte("SK"), nil },
		jwt.WithTimeFunc(func() time.Time { return now }), jwt.WithValidMethods([]string{"HS512"}))
	require.NoError(t, err)
	assert.Equal(t, "AK", claims.Subject)
	assert.Equal(t, cli.PrometheusTokenIssuer, claims.Issuer)
	assert.True(t, now.Add(time.Hour).Equal(claims.ExpiresAt.Time))
	assert.Equal(t, 2, strings.Count(token, "."))

	for _, tc := range []struct{ ak, sk string }{{"", "SK"}, {"AK", ""}} {
		_, err := cli.SignPrometheusToken(tc.ak, tc.sk, now, now.Add(time.Hour))
		require.ErrorIs(t, err, cli.ErrUsage)
	}
	_, err = cli.SignPrometheusToken("AK", "SK", now, now)
	require.ErrorIs(t, err, cli.ErrUsage)
}
