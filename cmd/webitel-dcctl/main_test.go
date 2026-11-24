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

func TestArbiterNotImplemented(t *testing.T) {
	err := newApp().Run([]string{"webitel-dcctl", "arbiter", "--config", "../../examples/arbiter.yaml"})
	assert.ErrorIs(t, err, errNotImplemented)
}

// The example config names the certificates of a real node, so serve gets
// past the config and stops at the runtime.
func TestServeStartsRuntime(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")

	err := newApp().Run([]string{"webitel-dcctl", "serve", "--config", "../../examples/config.yaml"})
	assert.ErrorContains(t, err, "tls.cert, tls.key: open /etc/ssl/app/dc_a_nginx.pem")
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
		t.Run(tt.cmd+"/missing_config", func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", tt.cmd, "--config", t.TempDir() + "/missing.yaml"})
			assert.ErrorIs(t, err, fs.ErrNotExist)
		})
		t.Run(tt.cmd+"/invalid_config", func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", tt.cmd, "--config", "../../go.mod"})
			assert.ErrorContains(t, err, "parse config")
		})
		t.Run(tt.cmd+"/default_path", func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", tt.cmd})
			assert.ErrorContains(t, err, tt.defaultPath)
		})
	}
}
