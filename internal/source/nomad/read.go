package nomad

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/nomad/api"

	"github.com/webitel/webitel-dcctl/internal/source"
)

const (
	// stageMetaKey is the job meta key that makes a job managed.
	stageMetaKey = "dcctl_stage"
	// servicesMetaKey is the node meta key with the services a node can run.
	servicesMetaKey = "services"

	// nodeInfoParallelism bounds the node reads running at once.
	nodeInfoParallelism = 8
)

// Jobs returns the jobs of the cluster: managed ones (with meta.dcctl_stage) and the
// rest. Managed jobs come ordered by stage and ID, those with an invalid stage last;
// the rest are ordered by ID. A job whose stage is invalid stays in the managed list
// with [Job.StageErr] set: it is a problem of the job, not of the endpoint, and
// dropping it would hide a job dcctl is meant to control.
//
// Nomad older than 1.5.0 does not list job meta and would show every job as unmanaged,
// so such a server is reported as invalid instead.
func (c *Client) Jobs(ctx context.Context) (managed, unmanaged []Job, err error) {
	err = c.each(ctx, func(ctx context.Context, nc *api.Client) error {
		if err := c.checkVersion(ctx, nc); err != nil {
			return err
		}

		stubs, err := request(ctx, c, func(ctx context.Context) ([]*api.JobListStub, error) {
			opts := &api.JobListOptions{Fields: &api.JobListFields{Meta: true}}
			stubs, _, err := nc.Jobs().ListOptions(opts, c.query(ctx))

			return stubs, err
		})
		if err != nil {
			return err
		}

		managed, unmanaged = nil, nil

		for _, s := range stubs {
			job := Job{
				ID: s.ID, Name: s.Name, Type: s.Type, Status: s.Status, Stop: s.Stop,
				ModifyIndex: s.ModifyIndex, JobModifyIndex: s.JobModifyIndex,
			}

			raw, ok := s.Meta[stageMetaKey]
			if !ok {
				unmanaged = append(unmanaged, job)

				continue
			}

			job.Stage, job.StageErr = parseStage(s.ID, raw)
			managed = append(managed, job)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	slices.SortFunc(managed, func(a, b Job) int {
		if (a.StageErr != nil) != (b.StageErr != nil) {
			if a.StageErr != nil {
				return 1
			}

			return -1
		}

		if a.Stage != b.Stage {
			return a.Stage - b.Stage
		}

		return strings.Compare(a.ID, b.ID)
	})
	slices.SortFunc(unmanaged, func(a, b Job) int { return strings.Compare(a.ID, b.ID) })

	return managed, unmanaged, nil
}

func parseStage(jobID, raw string) (int, error) {
	stage, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || stage < 1 {
		return 0, fmt.Errorf("%w: job %q has meta.%s %q", ErrInvalidStage, jobID, stageMetaKey, raw)
	}

	return stage, nil
}

// checkVersion fails with an invalid response when the server is older than
// [minJobMetaVersion]. An endpoint that passed is not asked again.
func (c *Client) checkVersion(ctx context.Context, nc *api.Client) error {
	addr := nc.Address()

	c.mu.Lock()
	ok := c.versionOK[addr]
	c.mu.Unlock()

	if ok {
		return nil
	}

	build, err := c.build(ctx, nc)
	if err != nil {
		return err
	}

	if !versionAtLeast(build, minJobMetaVersion) {
		return fmt.Errorf("%w: nomad %q lists no job meta, %d.%d.0 or newer is required",
			source.ErrInvalidResponse, build, minJobMetaVersion[0], minJobMetaVersion[1])
	}

	c.mu.Lock()
	c.versionOK[addr] = true
	c.mu.Unlock()

	return nil
}

// Version returns the version of a Nomad server of the datacenter, such as "1.7.2+ent".
func (c *Client) Version(ctx context.Context) (string, error) {
	var version string

	err := c.each(ctx, func(ctx context.Context, nc *api.Client) error {
		build, err := c.build(ctx, nc)
		version = build

		return err
	})
	if err != nil {
		return "", err
	}

	return version, nil
}

// build reads the version of the server nc talks to.
func (c *Client) build(ctx context.Context, nc *api.Client) (string, error) {
	self, err := request(ctx, c, func(ctx context.Context) (api.AgentSelf, error) {
		var self api.AgentSelf

		_, err := nc.Raw().Query("/v1/agent/self", &self, c.query(ctx))

		return self, err
	})
	if err != nil {
		return "", err
	}

	return self.Member.Tags["build"], nil
}

// versionAtLeast compares the major and minor of a version such as "1.7.2+ent".
// An unparsable version is too old: the check exists to stop a silent misreading.
func versionAtLeast(v string, lowest [2]int) bool {
	core, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), "+")
	core, _, _ = strings.Cut(core, "-")

	parts := strings.Split(core, ".")
	if len(parts) < 2 {
		return false
	}

	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])

	if err1 != nil || err2 != nil {
		return false
	}

	return major > lowest[0] || (major == lowest[0] && minor >= lowest[1])
}

// Allocs returns the allocations of a job, those of an earlier job with the same ID
// included: after a purge and a new register they may still run.
func (c *Client) Allocs(ctx context.Context, jobID string) ([]Alloc, error) {
	var allocs []Alloc

	err := c.each(ctx, func(ctx context.Context, nc *api.Client) error {
		stubs, err := request(ctx, c, func(ctx context.Context) ([]*api.AllocationListStub, error) {
			stubs, _, err := nc.Jobs().Allocations(jobID, true, c.query(ctx))

			return stubs, err
		})
		if err != nil {
			return err
		}

		allocs = make([]Alloc, 0, len(stubs))

		for _, s := range stubs {
			a := Alloc{
				ID: s.ID, JobID: s.JobID, TaskGroup: s.TaskGroup, NodeID: s.NodeID,
				ClientStatus: s.ClientStatus, DesiredStatus: s.DesiredStatus,
			}

			if len(s.TaskStates) > 0 {
				a.TaskStates = make(map[string]TaskState, len(s.TaskStates))
			}

			for name, ts := range s.TaskStates {
				if ts == nil {
					continue
				}

				a.TaskStates[name] = TaskState{
					State: ts.State, Failed: ts.Failed, Restarts: ts.Restarts, StartedAt: ts.StartedAt,
				}
			}

			allocs = append(allocs, a)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return allocs, nil
}

// Nodes returns the client nodes of the cluster. The node list carries no meta, so each
// node is read once more for meta.services.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var nodes []Node

	err := c.each(ctx, func(ctx context.Context, nc *api.Client) error {
		stubs, err := request(ctx, c, func(ctx context.Context) ([]*api.NodeListStub, error) {
			stubs, _, err := nc.Nodes().List(c.query(ctx))

			return stubs, err
		})
		if err != nil {
			return err
		}

		nodes = make([]Node, len(stubs))
		errs := make([]error, len(stubs))

		var wg sync.WaitGroup

		sem := make(chan struct{}, nodeInfoParallelism)

		for i, s := range stubs {
			wg.Go(func() {
				sem <- struct{}{}

				defer func() { <-sem }()

				info, err := request(ctx, c, func(ctx context.Context) (*api.Node, error) {
					info, _, err := nc.Nodes().Info(s.ID, c.query(ctx))

					return info, err
				})
				if err != nil {
					errs[i] = err

					return
				}

				nodes[i] = Node{
					ID: s.ID, Name: s.Name, Datacenter: s.Datacenter, Status: s.Status,
					SchedulingEligibility: s.SchedulingEligibility, Drain: s.Drain,
					Services: splitServices(info.Meta[servicesMetaKey]),
				}
			})
		}

		wg.Wait()

		return errors.Join(errs...)
	})
	if err != nil {
		return nil, err
	}

	return nodes, nil
}

func splitServices(v string) []string {
	var out []string

	for part := range strings.SplitSeq(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// Plan dry-runs the registration of the job as Start would register it, with Stop=false.
func (c *Client) Plan(ctx context.Context, jobID string) (PlanResult, error) {
	var res PlanResult

	err := c.each(ctx, func(ctx context.Context, nc *api.Client) error {
		job, err := c.readJob(ctx, nc, jobID)
		if err != nil {
			return err
		}

		job.Stop = ptr(false)

		resp, err := request(ctx, c, func(ctx context.Context) (*api.JobPlanResponse, error) {
			resp, _, err := nc.Jobs().PlanOpts(job, &api.PlanOptions{}, (&api.WriteOptions{}).WithContext(ctx))

			return resp, err
		})
		if err != nil {
			return err
		}

		res = PlanResult{FailedTGAllocs: slices.Sorted(maps.Keys(resp.FailedTGAllocs))}

		return nil
	})

	return res, err
}

// Endpoints probes every healthy Nomad server of the datacenter, in discovery order.
// A server with a certificate the CA does not vouch for comes back with reason tls.
func (c *Client) Endpoints(ctx context.Context) ([]EndpointStatus, error) {
	addrs, err := c.addrs(ctx)
	if err != nil {
		return nil, err
	}

	statuses := make([]EndpointStatus, len(addrs))

	var wg sync.WaitGroup

	for i, addr := range addrs {
		wg.Go(func() {
			statuses[i] = EndpointStatus{Addr: addr, Err: c.try(ctx, addr, c.probe)}
		})
	}

	wg.Wait()

	return statuses, nil
}

func (c *Client) probe(ctx context.Context, nc *api.Client) error {
	_, err := request(ctx, c, func(ctx context.Context) (struct{}, error) {
		var leader string

		_, err := nc.Raw().Query("/v1/status/leader", &leader, (&api.QueryOptions{}).WithContext(ctx))

		return struct{}{}, err
	})

	return err
}

// readJob returns the job as registered, with its JobModifyIndex.
func (c *Client) readJob(ctx context.Context, nc *api.Client, jobID string) (*api.Job, error) {
	job, err := request(ctx, c, func(ctx context.Context) (*api.Job, error) {
		job, _, err := nc.Jobs().Info(jobID, (&api.QueryOptions{}).WithContext(ctx))

		return job, err
	})
	if err != nil {
		return nil, err
	}

	if job.JobModifyIndex == nil {
		return nil, fmt.Errorf("%w: job %q has no JobModifyIndex", source.ErrInvalidResponse, jobID)
	}

	return job, nil
}

func ptr[T any](v T) *T { return &v }
