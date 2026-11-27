// Package scenario builds the plans of the dcctl actions for the operation engine and
// rebuilds the steps of an interrupted operation for its resume.
//
// A scenario turns an action and the state its preflight was decided on into an
// [operation.Plan]. Every step reads what it acts on when it runs, so that it can run
// again after the operation was interrupted, and passes a guard to the primitives that
// refuses once the lock is lost or the voters no longer hold the record the step acts
// under.
package scenario

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// ErrCannotPlan means the state the preflight was decided on gives no plan for the
// action, as when the active datacenter is not known; the preflight refuses such an
// action too.
var ErrCannotPlan = errors.New("action cannot be planned")

// Apps stops and starts the managed jobs of a datacenter; [*apps.Apps] is one.
type Apps interface {
	StopFrom(ctx context.Context, dc apps.Datacenter, from int, opts apps.Options) (apps.Result, error)
	StartUpTo(ctx context.Context, dc apps.Datacenter, to int, opts apps.Options) (apps.Result, error)
	StopFromFailover(ctx context.Context, dc apps.Datacenter, from int, opts apps.Options) (apps.Result, error)
}

// Patroni changes the Patroni clusters; [*pgctl.Controller] is one.
type Patroni interface {
	Demote(ctx context.Context, cl, target pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error)
	Catchup(ctx context.Context, demoted, target pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error)
	NoPrimary(ctx context.Context, clusters, others []pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error)
	Promote(ctx context.Context, cl pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error)
	Repoint(ctx context.Context, standby, primary pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error)
	Follow(ctx context.Context, standby, primary pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error)
	Leader(ctx context.Context, cl pgctl.Cluster) (patroni.Member, error)
	Divergence(ctx context.Context, standby, primary pgctl.Cluster) (patroni.Divergence, error)
	DemoteLoss(ctx context.Context, primary, standby pgctl.Cluster) (int64, error)
}

// Coordinator reads and writes dcctl/state on the voters; [*coord.Coordinator] is one.
type Coordinator interface {
	Read(ctx context.Context) (coord.View, error)
	Advance(ctx context.Context, from, to kv.State, opts coord.AdvanceOptions) (coord.WriteResult, error)
	SetTimeline(ctx context.Context, rec coord.RecordKey, timeline int64) (coord.WriteResult, error)
}

// Verifier runs the checks of an action anew; [*preflight.Runner] is one.
type Verifier interface {
	Run(ctx context.Context, req preflight.Request) (check.Verdict, error)
}

// Datacenter is what a scenario changes in one datacenter. Its Nomad must read
// consistently, see [apps.Nomad].
type Datacenter struct {
	Nomad   apps.Nomad
	Patroni pgctl.Patroni
}

// Deps is what the scenarios act through.
type Deps struct {
	Apps        Apps
	Patroni     Patroni
	Coordinator Coordinator
	Verifier    Verifier
	// Datacenters are the configured datacenters by name.
	Datacenters map[string]Datacenter
}

func (d Deps) validate() error {
	switch {
	case d.Apps == nil || d.Patroni == nil:
		return errors.New("scenario needs the apps and the patroni primitives")
	case d.Coordinator == nil || d.Verifier == nil:
		return errors.New("scenario needs the coordinator and the verifier")
	case len(d.Datacenters) == 0:
		return errors.New("scenario needs the datacenters")
	}

	for name, dc := range d.Datacenters {
		if dc.Nomad == nil || dc.Patroni == nil {
			return fmt.Errorf("datacenter %s needs Nomad and Patroni", name)
		}
	}

	return nil
}

func (d Deps) apps(dc string) apps.Datacenter {
	return apps.Datacenter{Name: dc, Nomad: d.Datacenters[dc].Nomad}
}

func (d Deps) cluster(dc string) pgctl.Cluster {
	return pgctl.Cluster{Datacenter: dc, Patroni: d.Datacenters[dc].Patroni}
}

func (d Deps) clusters(dcs ...string) []pgctl.Cluster {
	out := make([]pgctl.Cluster, 0, len(dcs))
	for _, dc := range dcs {
		out = append(out, d.cluster(dc))
	}

	return out
}

// known checks that every datacenter of dcs is configured.
func (d Deps) known(dcs ...string) error {
	for _, dc := range dcs {
		if _, ok := d.Datacenters[dc]; !ok {
			return fmt.Errorf("datacenter %q is not configured", dc)
		}
	}

	return nil
}

// interrupt marks err as one after which the operator decides: the engine interrupts
// the operation instead of rolling it back.
func interrupt(err error) error { return operation.Interrupt(err) }

// primaryElsewhere interrupts err when it is a primary found in a datacenter other than
// a: promoting a again would make two primaries.
func primaryElsewhere(err error, a string) error {
	var pe *pgctl.PrimaryError
	if errors.As(err, &pe) && pe.Datacenter != a {
		return interrupt(err)
	}

	return err
}

// stopping reports whether err ends a step because the operation must stop: the lock is
// lost, the voters changed under it, or the instance stops.
func stopping(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, apps.ErrGuard) || errors.Is(err, pgctl.ErrGuard) ||
		errors.Is(err, coord.ErrLockLost)
}

func operationID(name string) string { return strings.TrimPrefix(name, "operations/") }

func datacenterID(name string) string { return strings.TrimPrefix(name, "datacenters/") }

func appsDetail(p apps.Progress) string {
	var waiting []string

	for _, j := range p.Stage.Jobs {
		if j.State != apps.JobStopped && j.State != apps.JobHealthy {
			waiting = append(waiting, fmt.Sprintf("%s %s", j.ID, j.State))
		}
	}

	if len(waiting) == 0 {
		return fmt.Sprintf("%s apps of %s, stage %d: %s", p.Action, p.Datacenter, p.Stage.Number, p.Stage.State)
	}

	return fmt.Sprintf("%s apps of %s, stage %d: waiting for %s", p.Action, p.Datacenter, p.Stage.Number,
		strings.Join(waiting, ", "))
}

func patroniDetail(p pgctl.Progress) string {
	s := fmt.Sprintf("%s %s: %s", p.Action, p.Datacenter, p.Phase)

	if p.Detail != "" {
		s += ": " + p.Detail
	}

	if p.Lag != nil {
		s += fmt.Sprintf(" (lag %d bytes)", *p.Lag)
	}

	return s
}

// core builds the steps the scenarios share.
type core struct {
	d Deps
}
