package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistersAndGathers(t *testing.T) {
	reg := New(Deps{}, WithoutRuntimeCollectors())
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: Namespace, Name: "test_total"})
	require.NoError(t, reg.Register(c))
	c.Inc()
	families, err := reg.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	assert.Equal(t, "nexo_test_total", families[0].GetName())
	assert.Equal(t, float64(1), families[0].GetMetric()[0].GetCounter().GetValue())

	full := New(Deps{})
	families, err = full.Gather()
	require.NoError(t, err)
	assert.NotEmpty(t, families, "runtime collectors should produce metrics")
}
