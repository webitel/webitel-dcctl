package nomad

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/nomad/api"
)

// Stop deregisters the job without purging it: it stays registered with Stop=true.
func (c *Client) Stop(ctx context.Context, jobID string) error {
	return c.each(ctx, func(ctx context.Context, nc *api.Client) error {
		_, err := request(ctx, c, func(ctx context.Context) (string, error) {
			evalID, _, err := nc.Jobs().Deregister(jobID, false, (&api.WriteOptions{}).WithContext(ctx))

			return evalID, err
		})

		return err
	})
}

// Start registers the current version of the job again with Stop=false. The register
// carries EnforceIndex with the JobModifyIndex it read, so a concurrent change to the job
// is not overwritten: the job is read again and the register retried, up to the attempts
// in [Options]. A job that disappears meanwhile ends the retries with its 404.
//
// check, when not nil, runs before every attempt, the retries and other endpoints
// included: a retry registers the job over whatever changed it, so the caller confirms
// each time that it may still start the job. An error from check ends Start with that
// error, and no other endpoint is tried.
//
// Registering the job again drops its Submission (the HCL source) and does not restore
// scaling policies; Webitel jobs have no scaling block, so that is acceptable.
func (c *Client) Start(ctx context.Context, jobID string, check func(context.Context) error) error {
	return c.each(ctx, func(ctx context.Context, nc *api.Client) error {
		var err error

		for range c.attempts {
			if check != nil {
				if err := check(ctx); err != nil {
					return &checkError{err: err}
				}
			}

			var job *api.Job

			job, err = c.readJob(ctx, nc, jobID)
			if err != nil {
				return err
			}

			job.Stop = ptr(false)
			opts := &api.RegisterOptions{EnforceIndex: true, ModifyIndex: *job.JobModifyIndex}

			_, err = request(ctx, c, func(ctx context.Context) (*api.JobRegisterResponse, error) {
				resp, _, err := nc.Jobs().RegisterOpts(job, opts, (&api.WriteOptions{}).WithContext(ctx))

				return resp, err
			})
			if !errors.Is(err, ErrIndexConflict) {
				return err
			}
		}

		// The error stays unclassified: a job that keeps changing is not an unreachable endpoint.
		return fmt.Errorf("start job %q: %d attempts: %w", jobID, c.attempts, err)
	})
}

// checkError is the refusal of the check of [Client.Start]: a decision of the caller, not
// a failure of the endpoint, so it is neither classified nor tried on another endpoint.
type checkError struct{ err error }

func (e *checkError) Error() string { return e.err.Error() }

func (e *checkError) Unwrap() error { return e.err }
