package coord

import (
	"context"
	"errors"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// KeepLease renews the lease of datacenter dc while the effective state names
// it, one [Coordinator.RenewLease] per generation, until ctx ends. It reads the
// voters every renewal interval; when the effective record is resolved, see
// [ActiveKnown], and names dc, it renews the lease of that generation, and
// when that renewal stops on a newer generation it reads again an interval
// later. So an instance that restarts in the active datacenter goes on
// renewing the lease it held. KeepLease returns nil when ctx ends and an error
// only for a lease period that does not fit the voter timeout.
func (c *Coordinator) KeepLease(ctx context.Context, dc string, period time.Duration, fencing bool) error {
	interval := period / config.LeaseRenewalsPerPeriod

	for {
		if h, ok := c.effectiveHolder(ctx, dc, fencing); ok {
			err := c.RenewLease(ctx, h, period)

			switch {
			case ctx.Err() != nil:
				return nil
			case err != nil && !errors.Is(err, ErrNotLeaseHolder):
				return err
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// effectiveHolder returns the lease holder of dc when the effective state
// names it.
func (c *Coordinator) effectiveHolder(ctx context.Context, dc string, fencing bool) (LeaseHolder, bool) {
	view, err := c.Read(ctx)
	if err != nil || !ActiveKnown(view) || len(view.Conflict) > 0 || view.Effective.State.ActiveDatacenter != dc {
		return LeaseHolder{}, false
	}

	h := LeaseHolder{Datacenter: dc, Generation: view.Effective.State.Generation, Fencing: fencing}

	c.log.InfoCtx(ctx, "lease renewal starts", logging.Str("holder", dc), logging.Int64("generation", h.Generation))

	return h, true
}
