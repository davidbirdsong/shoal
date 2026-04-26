package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterpolate(t *testing.T) {
	base := cmdLineInterp{port: 2020, fdport: 3, addr: "192.168.1.1"}

	tests := []struct {
		name   string
		interp cmdLineInterp
		input  []string
		want   []string
	}{
		{
			name:   "single port placeholder",
			interp: base,
			input:  []string{"foo", "bar", "{port}"},
			want:   []string{"foo", "bar", "2020"},
		},
		{
			name:   "single addr placeholder",
			interp: base,
			input:  []string{"{addr}"},
			want:   []string{"192.168.1.1"},
		},
		{
			name:   "single fdport placeholder",
			interp: base,
			input:  []string{"--fd={fdport}"},
			want:   []string{"--fd=3"},
		},
		{
			name:   "multiple placeholders in one arg",
			interp: base,
			input:  []string{"{addr}:{port}"},
			want:   []string{"192.168.1.1:2020"},
		},
		{
			name:   "all placeholders across multiple args",
			interp: base,
			input:  []string{"{addr}", "{port}", "{fdport}"},
			want:   []string{"192.168.1.1", "2020", "3"},
		},
		{
			name:   "no placeholders passes through unchanged",
			interp: base,
			input:  []string{"myapp", "--verbose", "--config=/etc/app.conf"},
			want:   []string{"myapp", "--verbose", "--config=/etc/app.conf"},
		},
		{
			name:   "empty slice",
			interp: base,
			input:  []string{},
			want:   []string{},
		},
		{
			name:   "placeholder embedded in flag value",
			interp: base,
			input:  []string{"--listen={addr}:{port}"},
			want:   []string{"--listen=192.168.1.1:2020"},
		},
		{
			name:   "unknown placeholder is left as-is",
			interp: base,
			input:  []string{"no-placeholders"},
			want:   []string{"no-placeholders"},
		},
		{
			name:   "different port value",
			interp: cmdLineInterp{port: 8080, fdport: 4, addr: "10.0.0.1"},
			input:  []string{"{addr}:{port}", "--fd={fdport}"},
			want:   []string{"10.0.0.1:8080", "--fd=4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := interPolate(tc.input, tc.interp.applyFormat)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestInterpolateErrors(t *testing.T) {
	base := cmdLineInterp{port: 2020, fdport: 3, addr: "192.168.1.1"}

	tests := []struct {
		name        string
		interp      cmdLineInterp
		input       []string
		errContains string
	}{
		{
			name:        "unknown placeholder",
			interp:      base,
			input:       []string{"{unknown}"},
			errContains: `unknown placeholder "{unknown}"`,
		},
		{
			name:        "unknown placeholder mixed with valid args",
			interp:      base,
			input:       []string{"myapp", "{unknown}", "{port}"},
			errContains: `unknown placeholder "{unknown}"`,
		},
		{
			name:        "unknown placeholder after valid placeholder",
			interp:      base,
			input:       []string{"{addr}", "{typo}"},
			errContains: `unknown placeholder "{typo}"`,
		},
		{
			name:        "multiple unknown placeholders — first one errors",
			interp:      base,
			input:       []string{"{bad1}", "{bad2}"},
			errContains: `unknown placeholder "{bad1}"`,
		},
		{
			name:        "unknown placeholder embedded in arg",
			interp:      base,
			input:       []string{"--listen={addr}:{nope}"},
			errContains: `unknown placeholder "{nope}"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := interPolate(tc.input, tc.interp.applyFormat)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.errContains)
			assert.Empty(t, got)
		})
	}
}
