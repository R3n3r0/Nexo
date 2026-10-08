package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCredentialStatus(t *testing.T) {
	tests := []struct {
		in      string
		want    CredentialStatus
		wantErr bool
	}{
		{in: "enabled", want: StatusEnabled},
		{in: "disabled", want: StatusDisabled},
		{in: "", wantErr: true},
		{in: "Enabled", wantErr: true},
		{in: "paused", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseCredentialStatus(tt.in)
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
