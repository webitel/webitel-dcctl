// Package status derives from one collection snapshot what dcctl reports about the
// installation: the role and health of every datacenter, its Patroni and apps, the
// voters, the quorum and the current operation. Everything here is a pure
// function of the snapshot; nothing asks a source.
//
// The active datacenter is only ever the one of the effective state of the voters.
// When the voters do not tell it, no datacenter is called active or standby. The last
// known data of a source that does not answer is shown, yet no state, health or check
// is ever derived from it as fine.
package status

import (
	"slices"
	"time"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// Config is what the model takes from the serve config.
type Config struct {
	// Interval is status.interval, the pause between collection cycles.
	Interval time.Duration
	// NodeMetaKey is the node_meta key that holds the datacenter of a node.
	NodeMetaKey string
	// MinHealthyTime is apps.min_healthy_time.
	MinHealthyTime time.Duration
	// PlannedMaxLagBytes is the lag a healthy standby stays within.
	PlannedMaxLagBytes int64
	// RabbitMQService is status.rabbitmq_service, see [config.Status.RabbitMQServiceFor].
	RabbitMQService string
	InfoServices    []string
}

// ConfigFrom returns the part of cfg the model uses.
func ConfigFrom(cfg *config.Serve) Config {
	return Config{
		Interval:           cfg.Status.Interval,
		NodeMetaKey:        cfg.Consul.NodeMetaKey,
		MinHealthyTime:     cfg.Apps.MinHealthyTime,
		PlannedMaxLagBytes: cfg.Patroni.PlannedMaxLagBytes,
		RabbitMQService:    cfg.Status.RabbitMQService,
		InfoServices:       cfg.Status.InfoServices,
	}
}

// Status is the installation as one dcctl instance sees it.
type Status struct {
	Observer      collect.Instance
	ObserveTime   time.Time
	CycleDuration time.Duration
	// Interval is the configured pause between collection cycles.
	Interval time.Duration
	Profile  config.Profile
	// Initialized is set when some voter holds dcctl/state, also one read from a Consul
	// without a leader, which does not vote. When it is not and a voter does not
	// answer, the state may still exist there.
	Initialized bool
	// DetectedActiveDatacenter is where the Patroni primary is (warm_standby) or the
	// leader (stretch). It is empty unless every cluster answers and shows exactly one
	// primary or leader in a known datacenter.
	DetectedActiveDatacenter string
	// Effective is the effective state of the voters when it tells the active
	// datacenter, see [ActiveKnown]: nil when there is none, or when the voters that do
	// not answer may hold a newer committed record. View tells which.
	Effective *kv.State
	// View is the evaluation of the voters: the effective record and the uncommitted,
	// undetermined and conflicting ones.
	View coord.View
	// Voters are in the order of the configured voters, the arbiter last.
	Voters    []Voter
	Quorum    Quorum
	Operation *dcctlv1.Operation
	// Datacenters are in name order.
	Datacenters []Datacenter
}

// Build derives the status from snap at now. now judges how long the tasks of the
// apps have been running. The lease and the alarms are not part of it: they depend on
// how their records and conditions change across cycles.
func Build(snap *collect.Snapshot, cfg Config, now time.Time) Status {
	evaluated, voters := voterStates(snap)
	view := coord.Evaluate(evaluated)
	initialized := view.Initialized || slices.ContainsFunc(voters, func(r readVoter) bool {
		return r.read.Status == coord.StatusPresent || r.read.Status == coord.StatusInvalid
	})

	st := Status{
		Observer:      snap.Observer,
		ObserveTime:   snap.ObserveTime,
		CycleDuration: snap.CycleDuration,
		Interval:      cfg.Interval,
		Profile:       snap.Profile,
		Initialized:   initialized,
		View:          view,
		Voters:        votersOf(voters, view, initialized),
		Quorum:        quorumOf(view),
		Operation:     operationOf(Operations(snap)),
	}

	if ActiveKnown(view) {
		s := view.Effective.State
		st.Effective = &s
	}

	b := builder{snap: snap, cfg: cfg, now: now, view: view, effective: st.Effective, initialized: initialized}
	st.DetectedActiveDatacenter = b.detectedActive()

	for _, d := range snap.Datacenters {
		st.Datacenters = append(st.Datacenters, b.datacenter(d))
	}

	return st
}

// ActiveKnown reports whether the effective record tells the active datacenter. It
// does not when the voters that do not answer may hold a newer committed record:
//
//   - a forced record may be effective below an undetermined one; when that one names
//     another datacenter, the active datacenter is not known;
//   - a forced record held by less than a majority tells it only when the voters that
//     are unreachable or hold an invalid record are not a majority themselves: such a
//     majority may have committed a newer generation that no voter that answered shows.
func ActiveKnown(v coord.View) bool {
	if v.Effective == nil || slices.ContainsFunc(v.Undetermined, func(r coord.Record) bool {
		return r.State.ActiveDatacenter != v.Effective.State.ActiveDatacenter
	}) {
		return false
	}

	unknown := 0

	for _, vs := range v.Voters {
		if vs.Status == coord.StatusUnreachable || vs.Status == coord.StatusInvalid {
			unknown++
		}
	}

	return len(v.Effective.Voters) >= v.Majority || unknown < v.Majority
}

// builder holds what the model of every datacenter is derived from.
type builder struct {
	snap      *collect.Snapshot
	cfg       Config
	now       time.Time
	view      coord.View
	effective *kv.State
	// initialized is [Status.Initialized].
	initialized bool
}

func (b *builder) warm() bool {
	return b.snap.Profile == config.ProfileWarmStandby
}
