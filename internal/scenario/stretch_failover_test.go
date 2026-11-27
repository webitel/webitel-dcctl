package scenario_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// stretchFailoverLost are the calls of a failover of stretch from dc_a, lost, to dc_b.
var stretchFailoverLost = []string{
	"failover stop dc_a from 3", "failover dc_b to b1", "start dc_b to 1", "start dc_b to 2", "start dc_b to all",
}

// stretchFailoverForward are the calls of a failover of stretch whose dc_a still answers
// Nomad, to dc_b.
var stretchFailoverForward = []string{
	"failover stop dc_a from 3", "failover stop dc_a from 2", "failover stop dc_a from 0",
	"failover dc_b to b1", "start dc_b to 1", "start dc_b to 2", "start dc_b to all",
}

// loseStretchA makes dc_a lost: its members of the cluster and its Nomad do not answer,
// and the cluster has no leader.
func (w *world) loseStretchA() {
	w.infra.pg(loseLeader)
	w.infra.fail("failover stop dc_a from 3", fmt.Errorf("stop apps of dc_a: %w: %w", apps.ErrNotListed, apps.ErrTimeout))
}

// stretchFailedOver is the record a failover of stretch to dc_b writes over generation
// 5, with the timeline of the new leader: no datacenter needs recovery.
func stretchFailedOver(op *dcctlv1.Operation, timeline int64) kv.State {
	return kv.State{Generation: 6, Revision: 1, ActiveDatacenter: dcB, OpID: operationID(op), PatroniTimeline: timeline}
}

func TestStretchFailover_sourceLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, stretchFailoverLost, w.infra.allCalls())

		st := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0)
		assert.Equal(t, succeeded, st.GetState())
		assert.True(t, st.GetPointOfNoReturn())
		assert.Equal(t, []string{
			"member a1 of dc_a is not read; it may be primary", "member a2 of dc_a is not read; it may be primary",
		}, st.GetWarnings())

		assert.Contains(t, stepOf(t, op, dcctlv1.Operation_Step_VERIFY, 0).GetWarnings(), unconfirmedStop)
		assert.True(t, allRunning(w.infra.dc(dcB)))
		assert.Equal(t, "b1", w.infra.pgLeader())
		assert.Equal(t, int64(6), w.infra.pgMember("b1").timeline)
		assert.Equal(t, stretchFailedOver(op, 6), withoutTime(w.voters[0].state(t)))
		assert.Equal(t, dcctlv1.Operation_QUORUM, op.GetMode())
	})
}

// Patroni elected a leader in dc_b itself: the failover moves the apps only.
func TestStretchFailover_leaderInTargetSkipsFailover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) { c.lead("b2") })

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, skipped, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetState())
		assert.Equal(t, []string{"failover stop dc_a from 3", "start dc_b to 1", "start dc_b to 2", "start dc_b to all"},
			w.infra.allCalls())
		assert.Equal(t, stretchFailedOver(op, 6), withoutTime(w.voters[0].state(t)))
	})
}

// Every candidate lags: the failover needs the loss of the first one confirmed.
func TestStretchFailover_lossConfirmed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) {
			c.member("b1").lag = lagOf(300)
			c.member("b2").lag = lagOf(500)
		})

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Start(t.Context(), w.failoverPlan(isolation))
		assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err), "300 bytes are not confirmed")

		_, err = e.Start(t.Context(), w.failoverPlan(&dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 300}))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assertDiscards(t, op, "b1", 300)
		assert.Equal(t, "b1", w.infra.pgLeader())
	})
}

// assertDiscards checks that the failover of op warns that it discards n bytes of WAL on
// promoting cand, and of what it does not count.
func assertDiscards(t *testing.T, op *dcctlv1.Operation, cand string, n int64) {
	t.Helper()

	warnings := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetWarnings()
	want := fmt.Sprintf("the failover to %s discards %d bytes of WAL; %s", cand, n, preflight.LossNotCounted)
	assert.Contains(t, warnings, want)
}

// startFailover starts the failover to dc_b with c on a running engine and returns the
// error of the start and the operation as it ended.
func (w *world) startFailover(c *dcctlv1.Confirmation) (*dcctlv1.Operation, error) {
	w.t.Helper()

	e := w.engine()
	stop := start(w.t, e)

	defer stop()

	synctest.Wait()

	_, err := e.Start(w.t.Context(), w.failoverPlan(c))

	synctest.Wait()

	return w.op(), err
}

// c1 of dc_c received 5000 bytes more WAL than b1, the candidate, whose lag reads 0 as
// the DCS keeps the last position of the lost leader: the failover discards them.
func TestStretchFailover_lossCountsMoreAdvancedSurvivor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) { c.member("c1").received = lagOf(15000) })

		_, err := w.startFailover(isolation)
		assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err))

		op, err := w.startFailover(&dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 5000})
		require.NoError(t, err)
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assertDiscards(t, op, "b1", 5000)
		assert.Equal(t, "b1", w.infra.pgLeader())
	})
}

// b1 stayed on timeline 4 while the cluster is on 5: promoting it would fork the history,
// so b2, on timeline 5, is the candidate.
func TestStretchFailover_candidateOnOldTimeline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) { c.member("b1").timeline = 4 })

		op, err := w.startFailover(&dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 20})
		require.NoError(t, err)
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, "b2", w.infra.pgLeader())
		assertDiscards(t, op, "b2", 20)
	})
}

// Every member lags more than maximum_lag_on_failover, so Patroni elects nobody: the
// failover promotes the candidate once the loss is confirmed.
func TestStretchFailover_everyMemberOverTheLagLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) {
			c.maxLag = lagOf(100)
			c.member("b1").lag = lagOf(300)
			c.member("b2").lag = lagOf(500)
			c.member("c1").lag = lagOf(400)
		})

		_, err := w.startFailover(&dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 299})
		assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err))

		op, err := w.startFailover(&dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 300})
		require.NoError(t, err)
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, "b1", w.infra.pgLeader())
		assertDiscards(t, op, "b1", 300)
	})
}

// Patroni elects b2 itself between the read of the step and its request: the step is
// interrupted, and the resume finds the leader in dc_b.
func TestStretchFailover_patroniElectsBeforeTheRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.hook("failover dc_b to b1", func(context.Context) error {
			w.infra.pg(func(c *stretchCluster) { c.lead("b2") })

			return nil
		})

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Start(t.Context(), w.failoverPlan(isolation))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())

		op = w.resume(e, operationID(first), isolatedOnly)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, succeeded, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetState())
		assert.Equal(t, "b2", w.infra.pgLeader())
	})
}

// The candidate lags more at the step than the preflight measured: the failover is
// interrupted, and its resume confirms the loss anew.
func TestStretchFailover_lossGrowsBeforeTheStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) {
			c.member("b1").lag = lagOf(300)
			c.member("b2").lag = lagOf(500)
		})
		w.infra.hook("failover stop dc_a from 3", func(context.Context) error {
			w.infra.pg(func(c *stretchCluster) {
				c.member("b1").lag = lagOf(400)
				c.member("b2").lag = lagOf(600)
			})

			return fmt.Errorf("stop apps of dc_a: %w: %w", apps.ErrNotListed, apps.ErrTimeout)
		})

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Start(t.Context(), w.failoverPlan(&dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 300}))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "max_discard_bytes of 400")
		assert.NotContains(t, w.infra.allCalls(), "failover dc_b to b1")

		id := operationID(first)

		_, err = e.Resume(t.Context(), operation.ResumeRequest{
			Operation: id, Initiator: initiator, Confirmations: check.Confirmations{Isolation: true, MaxDiscardBytes: 300},
		})
		assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err), "the resume measures 400 bytes")

		op = w.resume(e, id, operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true, MaxDiscardBytes: 400}})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, "b1", w.infra.pgLeader())
	})
}

// A failover of stretch interrupts, never rolls back, and never promotes next to another
// leader or primary.
func TestStretchFailover_interrupts(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(w *world)
		detail string
	}{
		{
			name: "leader_returns_in_source",
			setup: func(w *world) {
				w.infra.hook("failover stop dc_a from 3", func(context.Context) error {
					w.infra.pg(func(c *stretchCluster) {
						c.member("a1").unread = false
						c.lead("a1")
					})

					return nil
				})
			},
			detail: "the leader a1 runs in dc_a",
		},
		{
			name:   "leader_elsewhere",
			setup:  func(w *world) { w.infra.pg(func(c *stretchCluster) { c.lead("c1") }) },
			detail: "the leader c1 runs in dc_c",
		},
		{
			name: "consul_primary_of_unknown_datacenter",
			setup: func(w *world) {
				w.infra.pg(func(c *stretchCluster) { c.consulPrimaries = map[string][]string{"": {"10.0.9.9"}} })
			},
			detail: "consul instance 10.0.9.9 of a datacenter that is not known",
		},
		{
			name: "member_primary",
			setup: func(w *world) {
				w.infra.pg(func(c *stretchCluster) { c.member("c1").role = patroni.RolePrimary })
			},
			detail: "member c1 of dc_c",
		},
		{
			name: "consul_primary",
			setup: func(w *world) {
				w.infra.pg(func(c *stretchCluster) { c.consulPrimaries = map[string][]string{dcA: {"a1"}} })
			},
			detail: "consul instance a1 of dc_a",
		},
		{
			// What the candidates received became unknown after the preflight: the loss
			// is not known.
			name: "position_unknown",
			setup: func(w *world) {
				w.infra.hook("failover stop dc_a from 3", func(context.Context) error {
					w.infra.pg(func(c *stretchCluster) {
						c.member("b1").lag = nil
						c.member("b2").lag = nil
					})

					return nil
				})
			},
			detail: "the WAL a failover discards is not known",
		},
		{
			name: "timeline_unknown",
			setup: func(w *world) {
				w.infra.hook("failover stop dc_a from 3", func(context.Context) error {
					w.infra.pg(func(c *stretchCluster) { c.member("c1").timelineUnknown = true })

					return nil
				})
			},
			detail: "the timeline of c1 is not known",
		},
		{
			// The loss grows past the confirmed bound after the preflight.
			name: "loss_not_covered",
			setup: func(w *world) {
				w.infra.hook("failover stop dc_a from 3", func(context.Context) error {
					w.infra.pg(func(c *stretchCluster) { c.member("c1").received = lagOf(10100) })

					return nil
				})
			},
			detail: "max_discard_bytes of 100",
		},
		{
			// A member on a node of no configured datacenter does not answer: whose
			// isolation it is, is not known.
			name: "member_of_unknown_datacenter_not_read",
			setup: func(w *world) {
				w.infra.pg(func(c *stretchCluster) {
					c.members = append(c.members, &pgMember{name: "x1", dc: "dc_x", role: patroni.RoleReplica, timeline: 5, unread: true, priority: 1})
				})
			},
			detail: "member x1 of a datacenter that is not known is not read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newStretchWorld(t)
				w.infra.pg(loseLeader)
				tt.setup(w)

				op := w.runFailover(isolation)

				require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				assert.Contains(t, op.GetError().GetDetail(), tt.detail)
				assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetState())
				assert.False(t, slices.ContainsFunc(w.infra.allCalls(), func(c string) bool { return c == "failover dc_b to b1" }))
				assert.True(t, noneRunning(w.infra.dc(dcB)))
				assert.Equal(t, int64(6), w.voters[0].state(t).Generation, "the generation stays")
				assert.True(t, w.lockFree())
			})
		})
	}
}

// The instance crashes in each phase of the failover; the resume, with the isolation
// confirmed anew while the cluster has not failed over, finishes it.
func TestStretchFailover_crashResumes(t *testing.T) {
	tests := []struct {
		key string
		req operation.ResumeRequest
	}{
		{key: "failover stop dc_a from 2", req: isolatedOnly},
		{key: "failover dc_b to b1", req: isolatedOnly},
		{key: "start dc_b to 2"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newStretchWorld(t)
				w.infra.pg(loseLeader)

				op := w.crashFailoverAndResume(tt.key, tt.req)

				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

				i := slices.Index(stretchFailoverForward, tt.key)
				assert.Equal(t, slices.Concat(stretchFailoverForward[:i+1], stretchFailoverForward[i:]), w.infra.allCalls())
				assert.Equal(t, "b1", w.infra.pgLeader())
				assert.True(t, allRunning(w.infra.dc(dcB)))
				assert.Equal(t, stretchFailedOver(op, 6), withoutTime(w.voters[0].state(t)), "the generation and its timeline are written once")
			})
		})
	}
}

// The failover took effect before the crash: the resumed step finds the leader in dc_b
// and succeeds without another request.
func TestStretchFailover_resumeAfterFailoverTookEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.infra.pg(loseLeader)

		var saved map[string][]byte

		w.infra.hook("failover dc_b to b1", func(ctx context.Context) error {
			saved = w.rawOps()
			w.infra.pg(func(c *stretchCluster) { c.lead("b1") })
			<-ctx.Done()

			return context.Cause(ctx)
		})

		e := w.engine()
		stop := start(t, e)

		synctest.Wait()

		first, err := e.Start(t.Context(), w.failoverPlan(isolation))
		require.NoError(t, err)

		synctest.Wait()
		stop()

		require.NotNil(t, saved)
		w.setRawOps(saved)

		e = w.engine()
		stop = start(t, e)

		defer stop()

		synctest.Wait()

		op := w.resume(e, operationID(first), isolatedOnly)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, succeeded, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetState())
		assert.Equal(t, 1, countCalls(w.infra.allCalls(), "failover dc_b to b1"))
		assert.Equal(t, stretchFailedOver(op, 6), withoutTime(w.voters[0].state(t)))
	})
}

// The failover always needs the isolation of the old active datacenter (D4).
func TestStretchFailover_needsIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Start(t.Context(), w.failoverPlan(nil))
		assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err))
		assert.Empty(t, w.infra.allCalls())
	})
}

// c1 of dc_c answers but reports no WAL position, as with Postgres in a crash loop: like
// a member that does not answer, its WAL is not counted, and it is named.
func TestStretchFailover_memberWithoutWALIsNotCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) { c.member("c1").lag, c.member("c1").received = nil, nil })

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, "b1", w.infra.pgLeader())
		assert.Contains(t, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetWarnings(),
			"member c1 reports no WAL position, the WAL it holds is not counted")
	})
}

// c1 of dc_c does not answer: whatever it holds is not counted.
func TestStretchFailover_unreadMemberIsNotCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) {
			c.member("c1").unread = true
			c.member("c1").received = lagOf(99999)
		})

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, "b1", w.infra.pgLeader())
		assert.Contains(t, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetWarnings(),
			"member c1 of dc_c is not read; it may be primary")
	})
}

// c1 on timeline 4 holds WAL past the fork, off the history of timeline 5: the failover
// to b1 does not discard it.
func TestStretchFailover_survivorOnLowerTimelineIsNotALoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.loseStretchA()
		w.infra.pg(func(c *stretchCluster) {
			c.member("c1").timeline = 4
			c.member("c1").received = lagOf(50000)
		})

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, "b1", w.infra.pgLeader())

		for _, warning := range stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FAILOVER, 0).GetWarnings() {
			assert.NotContains(t, warning, "discards")
		}
	})
}
