package telemetry

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/infra/health"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/status"
)

type checkFunc func(c *snapshotChecks, ctx context.Context) error

func TestSnapshotChecks(t *testing.T) {
	var (
		consul  checkFunc = (*snapshotChecks).consul
		nomad   checkFunc = (*snapshotChecks).nomad
		patroni checkFunc = (*snapshotChecks).patroni
		arbiter checkFunc = (*snapshotChecks).arbiter
	)

	tests := []struct {
		name    string
		check   checkFunc
		profile config.Profile
		// modify changes the representative observation; noCycle observes none.
		modify  func(o *status.Observed)
		noCycle bool
		wantErr bool
	}{
		{name: "consul_ok", check: consul},
		{name: "consul_failing", check: consul, modify: func(o *status.Observed) {
			o.Status.Datacenters[0].Sources[0] = unreachable(status.SystemConsul)
		}, wantErr: true},
		{name: "consul_kv_not_answering", check: consul, modify: func(o *status.Observed) {
			o.Status.Voters[0].Reachable = false
		}, wantErr: true},
		{name: "consul_stretch_catalog_only", check: consul, profile: config.ProfileStretch, modify: func(o *status.Observed) {
			o.Status.Voters = []status.Voter{{Name: "consul", Type: status.VoterDatacenter, Reachable: false}}
		}},
		{name: "consul_unknown", check: consul, noCycle: true, wantErr: true},
		{name: "consul_own_datacenter_missing", check: consul, modify: func(o *status.Observed) {
			o.Status.Datacenters = o.Status.Datacenters[1:]
		}, wantErr: true},
		{name: "consul_other_datacenter_failing", check: consul, modify: func(o *status.Observed) {
			o.Status.Datacenters[1].Sources[0] = unreachable(status.SystemConsul)
		}},

		{name: "nomad_ok", check: nomad},
		{name: "nomad_failing", check: nomad, modify: func(o *status.Observed) {
			o.Status.Datacenters[0].Sources[1] = unreachable(status.SystemNomad)
		}, wantErr: true},
		{name: "nomad_unknown", check: nomad, noCycle: true, wantErr: true},

		{name: "patroni_ok", check: patroni},
		{name: "patroni_leader_not_answering", check: patroni, modify: func(o *status.Observed) {
			o.Status.Datacenters[0].Patroni.Members[0].Reachable = false
		}, wantErr: true},
		{name: "patroni_no_leader", check: patroni, modify: func(o *status.Observed) {
			o.Status.Datacenters[0].Patroni.Leader = ""
		}, wantErr: true},
		{name: "patroni_failing", check: patroni, modify: func(o *status.Observed) {
			o.Status.Datacenters[0].Sources[2] = unreachable(status.SystemPatroni)
		}, wantErr: true},
		{name: "patroni_unknown", check: patroni, noCycle: true, wantErr: true},
		{name: "patroni_stretch_leader_elsewhere", check: patroni, profile: config.ProfileStretch, modify: func(o *status.Observed) {
			o.Status.Datacenters[0].Patroni.Leader = ""
		}},
		{name: "patroni_stretch_failing", check: patroni, profile: config.ProfileStretch, modify: func(o *status.Observed) {
			o.Status.Datacenters[0].Sources[2] = unreachable(status.SystemPatroni)
		}, wantErr: true},

		{name: "arbiter_ok", check: arbiter, modify: func(o *status.Observed) {
			o.Status.Voters[2].Reachable = true
		}},
		{name: "arbiter_failing", check: arbiter, wantErr: true},
		{name: "arbiter_unknown", check: arbiter, noCycle: true, wantErr: true},
		{name: "arbiter_not_observed", check: arbiter, modify: func(o *status.Observed) {
			o.Status.Voters = o.Status.Voters[:2]
		}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := representative()
			if tt.modify != nil {
				tt.modify(obs)
			}

			if tt.noCycle {
				obs = nil
			}

			profile := tt.profile
			if profile == "" {
				profile = config.ProfileWarmStandby
			}

			c := &snapshotChecks{src: observing(obs), cfg: HealthConfig{Datacenter: "dc_a", Profile: profile, Arbiter: true}}

			err := tt.check(c, t.Context())
			assert.Equal(t, tt.wantErr, err != nil, "error: %v", err)
		})
	}
}

func checkKeys(s health.Snapshot) []string {
	keys := make([]string, 0, len(s.Checks))
	for _, c := range s.Checks {
		keys = append(keys, c.Key())
	}

	return keys
}

func checkResult(t *testing.T, s health.Snapshot, key string) health.CheckResult {
	t.Helper()

	i := slices.IndexFunc(s.Checks, func(c health.CheckResult) bool { return c.Key() == key })
	require.GreaterOrEqual(t, i, 0, "check %s", key)

	return s.Checks[i]
}

func TestRegisterHealth_staleCollector(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval, requestTimeout = 5 * time.Second, 3 * time.Second

		collector := collect.New(collect.Config{
			Profile:  config.ProfileWarmStandby,
			Instance: collect.Instance{Datacenter: "dc_a", Node: "node-a"},
			Status:   config.Status{Interval: interval, RequestTimeout: requestTimeout},
		}, collect.Sources{}, nil)

		reg := health.New(health.Config{Interval: time.Second, Timeout: 500 * time.Millisecond, MinUnready: time.Second}, nil)
		RegisterHealth(reg, collector.Check, observing(representative()),
			HealthConfig{Datacenter: "dc_a", Profile: config.ProfileWarmStandby, Arbiter: true})

		require.NoError(t, reg.Start(t.Context()))

		defer func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
			defer cancel()

			require.NoError(t, reg.Shutdown(ctx))
		}()

		time.Sleep(5 * time.Second)
		synctest.Wait()

		fresh := reg.Snapshot()
		assert.Equal(t, []string{"consul/local", "dcctl/arbiter", "dcctl/collector", "nomad/local", "patroni/local"}, checkKeys(fresh))
		assert.Equal(t, health.StatusOK, checkResult(t, fresh, "dcctl/collector").Status)
		assert.Equal(t, health.StatusFail, checkResult(t, fresh, "dcctl/arbiter").Status, "the arbiter does not answer")
		assert.Equal(t, health.StateDegraded, fresh.State)

		// No cycle finishes for longer than 3 × (interval + request timeout).
		time.Sleep(3*(interval+requestTimeout) + 5*time.Second)
		synctest.Wait()

		stale := reg.Snapshot()
		assert.Equal(t, health.StatusFail, checkResult(t, stale, "dcctl/collector").Status)
		assert.Equal(t, health.GroupLiveness, checkResult(t, stale, "dcctl/collector").Group)
		assert.Equal(t, health.StateNotReady, stale.State)
	})
}

func TestRegisterHealth_noArbiter(t *testing.T) {
	reg := health.New(health.Config{}, nil)
	RegisterHealth(reg, func(context.Context) error { return nil }, observing(representative()),
		HealthConfig{Datacenter: "dc_a", Profile: config.ProfileWarmStandby})

	s := reg.Snapshot()
	assert.Equal(t, []string{"consul/local", "dcctl/collector", "nomad/local", "patroni/local"}, checkKeys(s))

	for _, c := range s.Checks {
		if c.Key() != "dcctl/collector" {
			assert.Equal(t, health.GroupInformational, c.Group, c.Key())
		}
	}
}
