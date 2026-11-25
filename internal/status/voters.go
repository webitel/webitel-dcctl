package status

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

var errNoLeader = errors.New("consul answered without a leader")

// VoterType is the storage behind a voter.
type VoterType string

const (
	// VoterDatacenter is the Consul KV of a datacenter, or the global Consul in stretch.
	VoterDatacenter VoterType = "datacenter"
	VoterArbiter    VoterType = "arbiter"
)

// Voter is the dcctl/state of one voter.
type Voter struct {
	// Name is the datacenter, [coord.ArbiterName], or [coord.StretchName] for the one
	// Consul of stretch.
	Name string
	Type VoterType
	// Datacenter is the datacenter of the Consul KV in warm_standby.
	Datacenter string
	// Reachable is set when the voter answered and takes part in the vote.
	Reachable bool
	// Reason is why the voter is unreachable or its record invalid.
	Reason source.Reason
	// Status is how the voter counts in the evaluation; a Consul without a leader
	// counts as unreachable.
	Status coord.Status
	// Generation, ActiveDatacenter and Forced are of the record read from the voter,
	// also when it came from a Consul without a leader.
	Generation       int64
	ActiveDatacenter string
	Forced           bool
	// Valid is set when the record is the effective one.
	Valid bool
	// Initialized is set for an arbiter that answers and holds the state.
	Initialized bool
	// Stale is set for a voter behind the effective state, which gets aligned.
	Stale bool
	// KeyMissing is set for a Consul that answered without dcctl/state while another
	// voter holds it.
	KeyMissing bool
}

// QuorumMode is how a generation can be written now.
type QuorumMode string

const (
	QuorumQuorum QuorumMode = "quorum"
	// QuorumForcedOnly allows only forced writes, with the isolation confirmation.
	QuorumForcedOnly QuorumMode = "forced_only"
)

// Quorum tells whether a generation can be written on a majority of the voters.
type Quorum struct {
	Available bool
	Mode      QuorumMode
	// ReachableCount counts the voters that answered and can take a write, see
	// [coord.View.Voting].
	ReachableCount int
	RequiredCount  int
	TotalCount     int
}

// readVoter is a voter as read and as it counts in the evaluation.
type readVoter struct {
	typ        VoterType
	datacenter string
	read       coord.VoterState
}

// voterStates returns the states of the voters to evaluate, in the configured order,
// and the voters as read.
func voterStates(snap *collect.Snapshot) ([]coord.VoterState, []readVoter) {
	eval := make([]coord.VoterState, 0, len(snap.Voters)+1)
	reads := make([]readVoter, 0, len(snap.Voters)+1)

	for _, v := range snap.Voters {
		rv := readVoter{typ: VoterDatacenter, read: kvState(voterName(snap.Profile, v), v.KV)}
		if snap.Profile == config.ProfileWarmStandby {
			rv.datacenter = v.Datacenter
		}

		vs := rv.read
		if v.KV.Reachable && !v.KV.Data.Meta.KnownLeader {
			vs = coord.VoterState{
				Name: vs.Name, Kind: coord.KindKV, Status: coord.StatusUnreachable, Reason: source.ReasonNoLeader, Err: errNoLeader,
			}
		}

		eval = append(eval, vs)
		reads = append(reads, rv)
	}

	if snap.Arbiter != nil {
		rv := readVoter{typ: VoterArbiter, read: arbiterState(*snap.Arbiter)}
		eval = append(eval, rv.read)
		reads = append(reads, rv)
	}

	return eval, reads
}

func voterName(profile config.Profile, v collect.Voter) string {
	if profile == config.ProfileStretch {
		return coord.StretchName
	}

	return v.Datacenter
}

func kvState(name string, src collect.Source[collect.KV]) coord.VoterState {
	switch e := src.Data.State; {
	case !src.Reachable:
		return coord.VoterState{
			Name: name, Kind: coord.KindKV, Status: coord.StatusUnreachable, Reason: src.Reason, Err: src.Err,
		}
	case e == nil:
		return coord.VoterState{Name: name, Kind: coord.KindKV, Status: coord.StatusAbsent}
	default:
		return coord.PresentState(name, coord.KindKV, e.Value, coord.Version(e.ModifyIndex))
	}
}

// arbiterState reads the arbiter as the coordinator does: one without the state record
// does not vote, whatever it answers.
func arbiterState(src collect.Source[collect.Arbiter]) coord.VoterState {
	switch rec := src.Data.State; {
	case !src.Reachable:
		return coord.VoterState{
			Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusUnreachable,
			Reason: src.Reason, Err: src.Err,
		}
	case !src.Data.Initialized || rec == nil:
		return coord.VoterState{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusUninitialized}
	default:
		return coord.PresentState(coord.ArbiterName, coord.KindArbiter, rec.Value, coord.Version(rec.Etag))
	}
}

func votersOf(reads []readVoter, view coord.View, initialized bool) []Voter {
	out := make([]Voter, len(reads))

	for i, r := range reads {
		vs := view.Voters[i]
		v := Voter{
			Name:       vs.Name,
			Type:       r.typ,
			Datacenter: r.datacenter,
			Reachable:  vs.Status != coord.StatusUnreachable,
			Reason:     vs.Reason,
			Status:     vs.Status,
			Stale:      vs.Stale,
			KeyMissing: vs.Kind == coord.KindKV && vs.Status == coord.StatusAbsent && initialized,
		}

		if r.read.Status == coord.StatusPresent {
			v.Generation = r.read.State.Generation
			v.ActiveDatacenter = r.read.State.ActiveDatacenter
			v.Forced = r.read.State.Forced
		}

		if view.Effective != nil {
			v.Valid = slices.Contains(view.Effective.Voters, vs.Name)
		}

		v.Initialized = r.typ == VoterArbiter && v.Reachable && vs.Status != coord.StatusUninitialized

		out[i] = v
	}

	return out
}

func quorumOf(view coord.View) Quorum {
	q := Quorum{
		Available:      view.Quorum,
		Mode:           QuorumForcedOnly,
		ReachableCount: view.Voting,
		RequiredCount:  view.Majority,
		TotalCount:     len(view.Voters),
	}

	if q.Available {
		q.Mode = QuorumQuorum
	}

	return q
}

// Operations returns dcctl/op of every voter KV in snap, in name order: a KV that
// does not answer is unreachable, a copy that cannot be decoded invalid.
func Operations(snap *collect.Snapshot) coord.OpView {
	copies := make([]coord.OpCopy, 0, len(snap.Voters))

	for _, v := range snap.Voters {
		c := coord.OpCopy{Name: voterName(snap.Profile, v)}

		switch e := v.KV.Data.Op; {
		case !v.KV.Reachable:
			c.Status, c.Reason, c.Err = coord.StatusUnreachable, v.KV.Reason, v.KV.Err
		case e == nil:
			c.Status = coord.StatusAbsent
		default:
			op := new(dcctlv1.Operation)
			if err := kv.DecodeProto(e.Value, op); err != nil {
				c.Status, c.Reason, c.Err = coord.StatusInvalid, source.ReasonInvalid, fmt.Errorf("decode %s: %w", kv.OpKey, err)
			} else {
				c.Status, c.Operation = coord.StatusPresent, op
			}
		}

		copies = append(copies, c)
	}

	slices.SortFunc(copies, func(a, b coord.OpCopy) int { return cmp.Compare(a.Name, b.Name) })

	return coord.OpView{Copies: copies}
}

// operationOf returns the operation that is not finished; nil also when the blocking
// copy cannot be read.
func operationOf(ops coord.OpView) *dcctlv1.Operation {
	if c := ops.Blocking(); c != nil {
		return c.Operation
	}

	return nil
}
