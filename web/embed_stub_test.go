//go:build !embedui

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAssetsStub(t *testing.T) {
	assets, ok := Assets()
	assert.False(t, ok, "the stub must report that the console is not embedded")
	assert.Nil(t, assets)
}
