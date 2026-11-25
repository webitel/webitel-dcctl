package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// RenewLease renews the lease of h every
// period/[config.LeaseRenewalsPerPeriod], starting at once, by
// [Coordinator.AcquireLease], until ctx ends or h is no longer the effective
// holder. Each round is bounded by the interval; the configuration makes it
// fit several voter timeouts.
//
// A round that does not reach a majority, or finds the effective state
// undetermined, is logged and the next round tries again. RenewLease
// returns nil when ctx ends and an error matching [ErrNotLeaseHolder] when
// the effective state names another datacenter or a higher generation: a
// new generation comes with an operation, which takes the lease itself.
func (c *Coordinator) RenewLease(ctx context.Context, h LeaseHolder, period time.Duration) error {
	if err := h.validate(); err != nil {
		return err
	}

	interval := period / config.LeaseRenewalsPerPeriod
	if interval <= c.timeout {
		return fmt.Errorf("lease renewal interval %s must be longer than the voter timeout %s", interval, c.timeout)
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	failing := false

	for {
		roundCtx, cancel := context.WithTimeout(ctx, interval)
		w, err := c.AcquireLease(roundCtx, h)

		cancel()

		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrNotLeaseHolder):
			c.log.InfoCtx(ctx, "lease renewal stopped",
				logging.Str("holder", h.Datacenter), logging.Int64("generation", h.Generation), logging.Err(err))

			return err
		case err != nil:
			failing = true

			c.log.WarnCtx(ctx, "lease not renewed",
				logging.Str("holder", h.Datacenter), logging.Int64("generation", h.Generation),
				logging.Int("written", w.Written()), logging.Int("majority", w.Majority), logging.Err(err))
		case failing:
			failing = false

			c.log.InfoCtx(ctx, "lease renewed again",
				logging.Str("holder", h.Datacenter), logging.Int64("generation", h.Generation),
				logging.Int("written", w.Written()))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
