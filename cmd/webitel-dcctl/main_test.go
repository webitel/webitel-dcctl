package main

import (
	"bytes"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/buildinfo"
	"github.com/webitel/webitel-dcctl/internal/config"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer

	app := newApp()
	app.Writer = &out

	require.NoError(t, app.Run([]string{"webitel-dcctl", "version"}))
	assert.Equal(t, buildinfo.Version+"\n", out.String())
}

func TestDaemonCommands(t *testing.T) {
	tests := []struct {
		cmd         string
		example     string
		defaultPath string
	}{
		{"serve", "../../examples/config.yaml", config.DefaultServePath},
		{"arbiter", "../../examples/arbiter.yaml", config.DefaultArbiterPath},
	}
	for _, tt := range tests {
		t.Run(tt.cmd+"/valid_config", func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", tt.cmd, "--config", tt.example})
			assert.ErrorIs(t, err, errNotImplemented)
		})
		t.Run(tt.cmd+"/missing_config", func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", tt.cmd, "--config", t.TempDir() + "/missing.yaml"})
			assert.ErrorIs(t, err, fs.ErrNotExist)
		})
		t.Run(tt.cmd+"/invalid_config", func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", tt.cmd, "--config", "../../go.mod"})
			require.Error(t, err)
			assert.NotErrorIs(t, err, errNotImplemented)
		})
		t.Run(tt.cmd+"/default_path", func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", tt.cmd})
			assert.ErrorContains(t, err, tt.defaultPath)
		})
	}
}
