package scenario

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

const (
	// syncStopUnconfirmed is the warning of a stop step of apps sync whose stage did not
	// stop in time.
	syncStopUnconfirmed = "the stop of the apps of the datacenter is not confirmed"
	// syncNomadSilent is the warning of a stop step of apps sync that could not list the
	// jobs of its datacenter: its Nomad does not answer.
	syncNomadSilent = "the Nomad of the datacenter does not answer, its apps are not stopped"
)

// SyncApps brings the managed apps in line with the decided record, in either profile:
// they stop in every datacenter but the active one, stages from the highest down, and
// then start in the active one, stages from the lowest up.
//
// Every datacenter stops on its own. A stage that does not stop in time is a warning
// and the next stage still stops, as long as the Nomad of the datacenter answers; a
// Nomad that does not list the jobs is a warning and the rest of its stages are skipped.
// A stage of the active datacenter that does not start ends the operation FAILED, with
// what the stage shows: nothing is rolled back. The apps change only while the lock is
// held and the voters hold the decided record the operation started from.
type SyncApps struct {
	core
}

// NewSyncApps returns apps sync acting through d.
func NewSyncApps(d Deps) (*SyncApps, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &SyncApps{core{d: d}}, nil
}

// syncPlan is apps sync with active the active datacenter: the stages of the other
// datacenters to stop, the highest first, and those of active to start.
type syncPlan struct {
	active string
	stops  []datacenterStages
	start  []int
}

type datacenterStages struct {
	dc     string
	stages []int
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict, so
// that a refusal can show the plan too. It fails with [ErrCannotPlan] when that state
// does not tell the active datacenter or it is not configured.
func (s *SyncApps) Plan(req *dcctlv1.SyncAppsRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	st := pre.Status

	base := coord.Base(st.View, false)
	if base == nil {
		return operation.Plan{}, fmt.Errorf("%w: the active datacenter is not known", ErrCannotPlan)
	}

	p := syncPlan{active: base.State.ActiveDatacenter}

	for _, d := range st.Datacenters {
		if d.Name == p.active {
			p.start = stagesOf(d.Nomad, false)
		} else {
			p.stops = append(p.stops, datacenterStages{dc: d.Name, stages: stagesOf(d.Nomad, true)})
		}
	}

	if p.start == nil {
		return operation.Plan{}, fmt.Errorf("%w: active datacenter %q is not configured", ErrCannotPlan, p.active)
	}

	if err := s.d.known(p.datacenters()...); err != nil {
		return operation.Plan{}, fmt.Errorf("%w: %w", ErrCannotPlan, err)
	}

	return operation.Plan{
		Type:               dcctlv1.Operation_SYNC_APPS,
		Target:             p.active,
		Request:            req,
		RequestID:          req.GetRequestId(),
		Initiator:          initiator,
		Preflight:          preflight.Request{Action: preflight.AppsSync},
		PreviousGeneration: base.State.Generation,
		PreviousActive:     p.active,
		Steps:              s.steps(p),
	}, nil
}

func (p syncPlan) datacenters() []string {
	out := make([]string, 0, len(p.stops)+1)
	out = append(out, p.active)

	for _, d := range p.stops {
		out = append(out, d.dc)
	}

	return out
}

// Resume rebuilds the steps of the interrupted apps sync op from its recorded steps, see
// [operation.Resumer].
func (s *SyncApps) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	p, err := syncPlanOf(op)
	if err == nil {
		err = s.d.known(p.datacenters()...)
	}

	if err != nil {
		return nil, fmt.Errorf("sync_apps %s: %w", op.GetName(), err)
	}

	return s.steps(p), nil
}

// syncPlanOf reads apps sync from the recorded steps of op.
func syncPlanOf(op *dcctlv1.Operation) (syncPlan, error) {
	p := syncPlan{active: datacenterID(op.GetTarget())}

	switch {
	case op.GetType() != dcctlv1.Operation_SYNC_APPS:
		return syncPlan{}, fmt.Errorf("operation of type %s is not of type %s", op.GetType(), dcctlv1.Operation_SYNC_APPS)
	case p.active == "" || op.GetPreviousGeneration() <= 0:
		return syncPlan{}, errors.New("no active datacenter or generation to act under")
	}

	for i, st := range op.GetSteps() {
		dc := datacenterID(st.GetDatacenter())

		//exhaustive:ignore // apps sync has no other steps
		switch st.GetType() {
		case dcctlv1.Operation_Step_APPS_STOP:
			if dc == "" || dc == p.active {
				return syncPlan{}, fmt.Errorf("step %d stops the apps of %q, not of an inactive datacenter", i, dc)
			}

			j := slices.IndexFunc(p.stops, func(d datacenterStages) bool { return d.dc == dc })
			if j < 0 {
				p.stops = append(p.stops, datacenterStages{dc: dc})
				j = len(p.stops) - 1
			}

			p.stops[j].stages = append(p.stops[j].stages, int(st.GetStage()))
		case dcctlv1.Operation_Step_APPS_START:
			if dc != p.active {
				return syncPlan{}, fmt.Errorf("step %d starts the apps of %q, not of the active datacenter %s", i, dc, p.active)
			}

			p.start = append(p.start, int(st.GetStage()))
		default:
			return syncPlan{}, fmt.Errorf("step %d of type %s is not a step of %s", i, st.GetType(), dcctlv1.Operation_SYNC_APPS)
		}
	}

	if len(p.start) == 0 {
		return syncPlan{}, errors.New("steps start no apps")
	}

	return p, nil
}

// steps returns the steps of p: the stop steps of every inactive datacenter, the last of
// which stops every stage, then the start steps of the active one, the last of which
// starts every stage, so that a job whose stage the plan did not see is handled too.
func (s *SyncApps) steps(p syncPlan) []operation.Step {
	steps := make([]operation.Step, 0, len(p.start)+len(p.stops)*3)

	for _, d := range p.stops {
		for i, stage := range d.stages {
			from := stage
			if i == len(d.stages)-1 {
				from = 0
			}

			steps = append(steps, operation.Step{
				Type: dcctlv1.Operation_Step_APPS_STOP, Stage: int32(stage), Datacenter: d.dc, // #nosec G115 -- stages come from stagesOf or a recorded int32
				Run: s.stopInactive(p.active, d.dc, from),
			})
		}
	}

	for i, stage := range p.start {
		to := stage
		if i == len(p.start)-1 {
			to = math.MaxInt
		}

		steps = append(steps, operation.Step{
			Type: dcctlv1.Operation_Step_APPS_START, Stage: int32(stage), Datacenter: p.active, // #nosec G115 -- stages come from stagesOf or a recorded int32
			Run: s.startActive(p.active, to),
		})
	}

	return steps
}

// stopInactive stops the apps of dc, a datacenter that is not active, from stage from up;
// from 0 stops every stage. What does not stop is a warning, see [SyncApps].
func (s *SyncApps) stopInactive(active, dc string, from int) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		if stopWarned(x.Operation(), dc, syncNomadSilent) {
			x.Skip(fmt.Sprintf("the Nomad of %s did not answer an earlier stop", dc))

			return nil
		}

		res, err := s.d.Apps.StopFrom(ctx, s.d.apps(dc), from, appsOptions(x, s.guardOn(x, active)))

		switch {
		case err == nil:
			warnApps(x, res)

			return nil
		case stopping(ctx, err):
			return err
		}

		x.Progress(fmt.Sprintf("apps of %s: %v", dc, err))

		if errors.Is(err, apps.ErrNotListed) {
			x.Warn(syncNomadSilent)
		} else {
			x.Warn(syncStopUnconfirmed)
		}

		return nil
	}
}

// startActive starts the apps of active up to stage to; math.MaxInt starts every stage.
// A job with an invalid stage refuses the start, which names it.
func (s *SyncApps) startActive(active string, to int) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		res, err := s.d.Apps.StartUpTo(ctx, s.d.apps(active), to, appsOptions(x, s.guardOn(x, active)))
		if err != nil {
			return fmt.Errorf("apps of %s do not start: %w", active, err)
		}

		warnApps(x, res)

		return nil
	}
}
