package clock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := NewFake(start)
	assert.Equal(t, start, f.Now())
	assert.Equal(t, start.Add(time.Minute), f.Advance(time.Minute))
	assert.Equal(t, start.Add(time.Minute), f.Now())
	f.Set(start)
	assert.Equal(t, start, f.Now())
}

func TestReal(t *testing.T) {
	before := time.Now()
	now := NewReal().Now()
	assert.False(t, now.Before(before))
}
