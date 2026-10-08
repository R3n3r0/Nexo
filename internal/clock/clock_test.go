package clock_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/clock"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := clock.NewFake(start)
	assert.Equal(t, start, f.Now())
	assert.Equal(t, start.Add(time.Minute), f.Advance(time.Minute))
	assert.Equal(t, start.Add(time.Minute), f.Now())
	f.Set(start)
	assert.Equal(t, start, f.Now())
}

func TestReal(t *testing.T) {
	before := time.Now()
	now := clock.NewReal().Now()
	assert.False(t, now.Before(before))
}
