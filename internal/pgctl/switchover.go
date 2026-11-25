package pgctl

import (
	"context"
	"fmt"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Switchover hands the leadership of the cluster of cl over to candidate, which must run
// in cl.Datacenter: POST /switchover to the leader. Patroni holds the request until the
// switch is done or given up; the condition is polled meanwhile, as the answer may be an
// error or a 200 for another member. Within patroni.timeouts.promote.
//
// A candidate that leads already, as after an attempt with an unknown outcome, is done
// without a request when it is the only primary; otherwise it fails the precondition.
func (c *Controller) Switchover(ctx context.Context, cl Cluster, candidate string, opts Options) (Result, error) {
	s := &switchover{c: c, cl: cl, candidate: candidate}

	return c.run(ctx, primitive{
		action: ActionSwitchover, dc: cl.Datacenter, timeout: c.cfg.Timeouts.Promote,
		prepare: s.prepare, send: s.send, skip: func() bool { return s.leads }, observe: s.observe,
	}, opts)
}

type switchover struct {
	c         *Controller
	cl        Cluster
	candidate string

	endpoint, leader string
	startTimeline    int64
	// leads is set when the candidate led before the request.
	leads bool
}

func (s *switchover) prepare(ctx context.Context, _ bool) (Observation, error) {
	members, err := s.cl.Patroni.Members(ctx)
	if err != nil {
		return Observation{Err: err}, err
	}

	obs := describe(members)

	if err := s.c.checkCandidate(ctx, s.cl, members, s.candidate); err != nil {
		return obs, err
	}

	l, ok := leaderOf(members)
	if !ok {
		return obs, errNoLeader
	}

	if l.Name == s.candidate {
		return s.alreadyLeads(ctx, members)
	}

	if err := ownEndpoint(l); err != nil {
		return obs, err
	}

	if err := usable(l); err != nil {
		return obs, err
	}

	switch {
	case l.Probe.Role == patroni.RoleStandbyLeader:
		return obs, precondition("leader %s is standby_leader: the cluster is a standby", l.Name)
	case l.Probe.Role != patroni.RolePrimary:
		return obs, fmt.Errorf("leader %s is %s", l.Name, l.Probe.Role)
	case l.Probe.Timeline == nil:
		return obs, fmt.Errorf("timeline of leader %s unknown", l.Name)
	}

	s.endpoint, s.leader, s.startTimeline = l.Endpoint, l.Name, *l.Probe.Timeline

	return obs, nil
}

// alreadyLeads checks a candidate that leads before the request: done when it is the
// only primary, a precondition failure otherwise.
func (s *switchover) alreadyLeads(ctx context.Context, members []patroni.Member) (Observation, error) {
	obs, cand, ok := candidateLeads(members, s.candidate)
	if !ok {
		if obs.Err != nil {
			return obs, obs.Err
		}

		return obs, precondition("%s leads already: %s", s.candidate, obs.Detail)
	}

	detail, err := s.c.soloPrimary(ctx, s.cl.Datacenter, members, cand)

	switch {
	case err != nil:
		return obs.miss(err, "%s", detail), err
	case detail != "":
		return obs.miss(nil, "%s", detail), precondition("%s leads already: %s", s.candidate, detail)
	}

	s.leads = true

	return obs, nil
}

func (s *switchover) send(ctx context.Context) error {
	_, err := s.cl.Patroni.Switchover(ctx, s.endpoint, s.leader, s.candidate)

	return err
}

func (s *switchover) observe(ctx context.Context) (Observation, bool, error) {
	members, err := s.cl.Patroni.Members(ctx)
	if err != nil {
		return readFailed(s.cl.Datacenter, err)
	}

	obs, cand, ok := candidateLeads(members, s.candidate)
	if !ok {
		return obs, false, nil
	}

	if !s.leads && *cand.Probe.Timeline <= s.startTimeline {
		return obs.unmet(nil, "%s still on timeline %d", cand.Name, *cand.Probe.Timeline)
	}

	if detail, err := s.c.soloPrimary(ctx, s.cl.Datacenter, members, cand); detail != "" {
		return obs.unmet(err, "%s", detail)
	}

	return obs, true, nil
}

// Failover promotes candidate, which must run in cl.Datacenter, in a cluster without a
// leader: POST /failover to the candidate. A cluster with a leader is refused. Like
// [Controller.Switchover] the condition is polled while Patroni holds the request. Within
// patroni.timeouts.promote.
func (c *Controller) Failover(ctx context.Context, cl Cluster, candidate string, opts Options) (Result, error) {
	f := &failover{c: c, cl: cl, candidate: candidate}

	return c.run(ctx, primitive{
		action: ActionFailover, dc: cl.Datacenter, timeout: c.cfg.Timeouts.Promote,
		prepare: f.prepare, send: f.send, observe: f.observe,
	}, opts)
}

type failover struct {
	c         *Controller
	cl        Cluster
	candidate string

	endpoint string
}

func (f *failover) prepare(ctx context.Context, _ bool) (Observation, error) {
	members, err := f.cl.Patroni.Members(ctx)
	if err != nil {
		return Observation{Err: err}, err
	}

	obs := describe(members)

	if l, ok := leaderOf(members); ok {
		return obs, precondition("%s leads the cluster", l.Name)
	}

	if err := f.c.checkCandidate(ctx, f.cl, members, f.candidate); err != nil {
		return obs, err
	}

	cand, _ := find(members, f.candidate)
	f.endpoint = cand.Endpoint

	return obs, nil
}

func (f *failover) send(ctx context.Context) error {
	_, err := f.cl.Patroni.Failover(ctx, f.endpoint, f.candidate)

	return err
}

func (f *failover) observe(ctx context.Context) (Observation, bool, error) {
	members, err := f.cl.Patroni.Members(ctx)
	if err != nil {
		return readFailed(f.cl.Datacenter, err)
	}

	obs, _, ok := candidateLeads(members, f.candidate)

	return obs, ok, nil
}

// checkCandidate checks that candidate is a member of the cluster that takes requests at
// its own host in cl and runs a supported Patroni.
func (c *Controller) checkCandidate(ctx context.Context, cl Cluster, members []patroni.Member, candidate string) error {
	cand, ok := find(members, candidate)
	if !ok {
		return precondition("no member %s", candidate)
	}

	if err := ownEndpoint(cand); err != nil {
		return err
	}

	if err := c.inDatacenter(ctx, cl.Datacenter, cand); err != nil {
		return err
	}

	return usable(cand)
}

// candidateLeads reports whether candidate leads by GET /cluster and is primary on a
// known timeline by its GET /patroni.
func candidateLeads(members []patroni.Member, candidate string) (Observation, patroni.Member, bool) {
	obs := describe(members)

	cand, ok := find(members, candidate)
	if !ok {
		return obs.miss(nil, "no member %s", candidate), cand, false
	}

	obs = obs.about(cand)
	obs.Lag = cand.Lag

	l, ok := leaderOf(members)

	switch {
	case !ok:
		return obs.miss(nil, "no leader"), cand, false
	case l.Name != candidate || l.Role != patroni.ClusterRoleLeader:
		return obs.miss(nil, "leader is %s", l.Name), cand, false
	case cand.Probe == nil:
		return obs.miss(cand.Err, "%s not read", candidate), cand, false
	case cand.Probe.Role != patroni.RolePrimary:
		return obs.miss(nil, "%s is %s", candidate, cand.Probe.Role), cand, false
	case cand.Probe.Timeline == nil:
		return obs.miss(nil, "timeline of %s unknown", candidate), cand, false
	}

	return obs, cand, true
}
