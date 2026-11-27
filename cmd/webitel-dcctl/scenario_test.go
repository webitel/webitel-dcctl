package main

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
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

// Serve plans and resumes the switch and the failover of its profile. Demote, repoint
// and mark-recovered are planned in both, for their preflight to refuse them in stretch,
// and resumed in warm_standby only.
func TestNewScenariosByProfile(t *testing.T) {
	tests := []struct {
		profile  config.Profile
		switchOf any
		failOf   any
		resumed  []dcctlv1.Operation_Type
	}{
		{
			profile: config.ProfileWarmStandby, switchOf: &scenario.Switch{}, failOf: &scenario.Failover{},
			resumed: []dcctlv1.Operation_Type{
				dcctlv1.Operation_SWITCH, dcctlv1.Operation_FAILOVER, dcctlv1.Operation_DEMOTE, dcctlv1.Operation_REPOINT,
				dcctlv1.Operation_MARK_RECOVERED,
			},
		},
		{
			profile: config.ProfileStretch, switchOf: &scenario.StretchSwitch{}, failOf: &scenario.StretchFailover{},
			resumed: []dcctlv1.Operation_Type{dcctlv1.Operation_SWITCH, dcctlv1.Operation_FAILOVER},
		},
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

			planners, resumers, err := newScenarios(cfg, operationDeps{clients: clients, coordinator: c}, &preflight.Runner{}, nomads)
			require.NoError(t, err)
			assert.IsType(t, tt.switchOf, planners.Switch)
			assert.IsType(t, tt.failOf, planners.Failover)
			assert.IsType(t, &scenario.Demote{}, planners.Demote)
			assert.IsType(t, &scenario.Repoint{}, planners.Repoint)
			assert.IsType(t, &scenario.MarkRecovered{}, planners.MarkRecovered)
			assert.ElementsMatch(t, tt.resumed, slices.Collect(maps.Keys(resumers)))
			assert.Same(t, planners.Switch, resumers[dcctlv1.Operation_SWITCH])
		})
	}
}
