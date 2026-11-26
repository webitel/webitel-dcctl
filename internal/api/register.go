package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/runtime"
)

// Options are what the API reads from.
type Options struct {
	// Address is listen.peer of this instance, shown as the observer address.
	Address    string
	Status     StatusSource
	Operations Operations
	// Events may be nil: WatchOperation then polls the copies of dcctl/op on
	// every instance.
	Events  OperationEvents
	Journal Journal
	// Alarms may be nil: GetStatus then reports no alarms.
	Alarms Alarms
	// Verifier may be nil: VerifyStatus then answers Unimplemented.
	Verifier Verifier
	// UI is the UI build, nil to serve no UI.
	UI fs.FS
	// Actions may be nil: the actions then answer Unimplemented.
	Actions *Actions
	Log     *logging.Logger
}

// Register registers the API services and their gateway routes on rt. Call
// it before rt.Run.
func Register(ctx context.Context, rt *runtime.Runtime, opts Options) error {
	switch {
	case opts.Status == nil:
		return errors.New("api: no status source")
	case opts.Operations == nil:
		return errors.New("api: no operations")
	case opts.Journal == nil:
		return errors.New("api: no journal")
	case opts.Log == nil:
		return errors.New("api: no logger")
	}

	actions, err := newActionServer(opts.Actions, opts.Status, opts.Operations, opts.Log)
	if err != nil {
		return err
	}

	dcs := NewDatacenterServer(opts.Status, opts.Address, opts.Log)
	dcs.actions = actions

	ops := NewOperationServer(opts.Operations, opts.Events, ctx.Done(), opts.Log)
	ops.actions = actions

	rt.RegisterService(&dcctlv1.StatusService_ServiceDesc,
		NewStatusServer(opts.Status, opts.Address, opts.Alarms, opts.Verifier, opts.Log))
	rt.RegisterService(&dcctlv1.DatacenterService_ServiceDesc, dcs)
	rt.RegisterService(&dcctlv1.OperationService_ServiceDesc, ops)
	rt.RegisterService(&dcctlv1.JournalService_ServiceDesc, NewJournalServer(opts.Journal, opts.Log))

	for _, h := range []runtime.GatewayHandler{
		dcctlv1.RegisterStatusServiceHandler,
		dcctlv1.RegisterDatacenterServiceHandler,
		registerOperationGateway,
		dcctlv1.RegisterJournalServiceHandler,
	} {
		if err := rt.RegisterGateway(ctx, h); err != nil {
			return fmt.Errorf("api: %w", err)
		}
	}

	if opts.UI != nil {
		h, err := NewUI(opts.UI, opts.Log)
		if err != nil {
			return err
		}

		rt.HandleUI(h)
	}

	return nil
}
