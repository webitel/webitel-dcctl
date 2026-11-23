package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExampleServe(t *testing.T) {
	got, err := LoadServe("../../examples/config.yaml")
	require.NoError(t, err)

	want := defaultServe()
	assert.Equal(t, want.Patroni.PlannedMaxLagBytes, got.Patroni.PlannedMaxLagBytes)
	assert.Equal(t, want.Patroni.Timeouts, got.Patroni.Timeouts)
	assert.Equal(t, want.Coordination, got.Coordination)
	assert.Equal(t, want.Apps, got.Apps)
	assert.Equal(t, want.Status, got.Status)
	assert.Equal(t, want.Journal, got.Journal)
}

func TestExampleArbiter(t *testing.T) {
	got, err := LoadArbiter("../../examples/arbiter.yaml")
	require.NoError(t, err)

	assert.Equal(t, defaultArbiter().DataDir, got.DataDir)
}
