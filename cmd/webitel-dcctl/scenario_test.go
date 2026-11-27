package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

// silentVoter is a voter that holds nothing; the scenarios are only built here.
type silentVoter struct{}

func (silentVoter) Name() string     { return "consul" }
func (silentVoter) Kind() coord.Kind { return coord.KindKV }

func (silentVoter) Get(context.Context, coord.Key) (coord.Entry, error) {
	return coord.Entry{}, coord.ErrNotFound
}

func (silentVoter) CAS(context.Context, coord.Key, []byte, coord.Version) error { return nil }

// Serve plans and resumes the switch and the failover of its profile.
func TestNewScenariosByProfile(t *testing.T) {
	tests := []struct {
		profile  config.Profile
		switchOf any
		failOf   any
	}{
		{profile: config.ProfileWarmStandby, switchOf: &scenario.Switch{}, failOf: &scenario.Failover{}},
		{profile: config.ProfileStretch, switchOf: &scenario.StretchSwitch{}, failOf: &scenario.StretchFailover{}},
	}

	for _, tt := range tests {
		t.Run(string(tt.profile), func(t *testing.T) {
			cfg, err := config.LoadServe("../../examples/config.yaml")
			require.NoError(t, err)

			cfg.TLS = testpki.New(t, "dcctl").Files(t)

			clients, err := consulClients(cfg)
			require.NoError(t, err)

			cfg.Profile = tt.profile

			c, err := coord.New([]coord.Voter{silentVoter{}}, time.Second, logging.New(logging.WithProvider(logtest.NewRecorder())))
			require.NoError(t, err)

			nomads := map[string]*nomad.Client{}

			for _, dc := range clients.Datacenters() {
				nc, err := nomad.New(nomad.Options{
					Datacenter: dc, Service: nomad.ServiceName(cfg.Profile, dc), CAFile: cfg.TLS.CA, Resolver: clients,
					RequestTimeout: time.Second, ConsistentReads: true,
				})
				require.NoError(t, err)

				nomads[dc] = nc
			}

			sw, fo, err := newScenarios(cfg, operationDeps{clients: clients, coordinator: c}, &preflight.Runner{}, nomads)
			require.NoError(t, err)
			assert.IsType(t, tt.switchOf, sw)
			assert.IsType(t, tt.failOf, fo)
		})
	}
}
