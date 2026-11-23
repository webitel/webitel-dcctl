package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/buildinfo"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer

	app := newApp()
	app.Writer = &out

	require.NoError(t, app.Run([]string{"webitel-dcctl", "version"}))
	assert.Equal(t, buildinfo.Version+"\n", out.String())
}

func TestStubCommands(t *testing.T) {
	for _, cmd := range []string{"serve", "arbiter"} {
		t.Run(cmd, func(t *testing.T) {
			err := newApp().Run([]string{"webitel-dcctl", cmd})
			assert.ErrorIs(t, err, errNotImplemented)
		})
	}
}
