package api

import (
	"cmp"
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

const defaultPageSize = 50

// OperationServer implements OperationService. Without actions, see
// [Options.Actions], resume and abandon answer Unimplemented.
//
// Operations are read from the copies of dcctl/op on every request, not from
// the last collection cycle, so an operation an action just started is found
// at once. dcctl/op holds the last operation of each KV only: older
// operations are in the journal, not here.
type OperationServer struct {
	dcctlv1.UnimplementedOperationServiceServer

	ops    Operations
	events OperationEvents
	stop   <-chan struct{}
	log    *logging.Logger

	polled       *sharedView
	pollInterval time.Duration
	keepalive    time.Duration
	actions      *actionServer
}

// NewOperationServer returns the OperationService of ops. events may be nil:
// WatchOperation then always polls the copies. Streams end when stop is
// closed.
func NewOperationServer(ops Operations, events OperationEvents, stop <-chan struct{}, log *logging.Logger) *OperationServer {
	return &OperationServer{
		ops: ops, events: events, stop: stop, log: log,
		polled:       &sharedView{ops: ops, ttl: watchPollInterval / 2},
		pollInterval: watchPollInterval, keepalive: watchKeepalive,
	}
}

// ListOperations returns the operations of the copies, newest first.
func (s *OperationServer) ListOperations(ctx context.Context, req *dcctlv1.ListOperationsRequest) (*dcctlv1.ListOperationsResponse, error) {
	after, err := decodePageToken(req.GetPageToken())
	if err != nil {
		return nil, err
	}

	view, err := s.ops.Read(ctx)
	if err != nil {
		return nil, rpcError(ctx, s.log, "operations cannot be read", err)
	}

	if !slices.ContainsFunc(view.Copies, readable) {
		return nil, apierr.KVUnavailable("no copy of dcctl/op is readable", unreadable(view))
	}

	ops := operations(view)
	size := cmp.Or(int(req.GetPageSize()), defaultPageSize)
	out := &dcctlv1.ListOperationsResponse{}

	for _, op := range ops {
		if after != nil && !after.before(op) {
			continue
		}

		if len(out.GetOperations()) == size {
			out.NextPageToken = encodePageToken(out.GetOperations()[size-1])

			break
		}

		out.Operations = append(out.Operations, op)
	}

	return out, nil
}

// GetOperation returns the operation from the copy that answers for it, see
// [coord.OpView.Of]. The operation is not found only when every copy was
// read: a copy that does not answer or cannot be decoded may hold it.
func (s *OperationServer) GetOperation(ctx context.Context, req *dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) {
	view, err := s.ops.Read(ctx)
	if err != nil {
		return nil, rpcError(ctx, s.log, "operation cannot be read", err)
	}

	return lookup(view, req.GetName())
}

// lookup returns the operation name as the copy that answers for it holds it;
// see [OperationServer.GetOperation] for when it is not found.
func lookup(view coord.OpView, name string) (*dcctlv1.Operation, error) {
	if c := view.Of(name); c != nil {
		return c.Operation, nil
	}

	if !slices.ContainsFunc(view.Copies, func(c coord.OpCopy) bool { return !readable(c) }) {
		return nil, operationNotFound(name)
	}

	return nil, apierr.KVUnavailable("the operation may be in a copy of dcctl/op that is not readable", unreadable(view))
}

// unreadable returns the datacenters whose copy of dcctl/op was not read. The
// global Consul of stretch is not a datacenter and is left out.
func unreadable(view coord.OpView) []string {
	var out []string

	for _, c := range view.Copies {
		if !readable(c) && c.Name != coord.StretchName {
			out = append(out, c.Name)
		}
	}

	return out
}

// readable reports whether the copy was read: it holds an operation or none.
func readable(c coord.OpCopy) bool {
	return c.Status == coord.StatusPresent || c.Status == coord.StatusAbsent
}

// operations returns every operation the copies hold, each once, newest
// first.
func operations(view coord.OpView) []*dcctlv1.Operation {
	var out []*dcctlv1.Operation

	for _, c := range view.Copies {
		if c.Status != coord.StatusPresent || slices.ContainsFunc(out, func(op *dcctlv1.Operation) bool {
			return op.GetName() == c.Operation.GetName()
		}) {
			continue
		}

		out = append(out, view.Of(c.Operation.GetName()).Operation)
	}

	slices.SortFunc(out, func(a, b *dcctlv1.Operation) int {
		return cmp.Or(b.GetCreateTime().AsTime().Compare(a.GetCreateTime().AsTime()), strings.Compare(b.GetName(), a.GetName()))
	})

	return out
}

// pagePosition is the last operation of a page: the next page continues with
// the operations listed after it.
type pagePosition struct {
	createTime time.Time
	name       string
}

// before reports whether op is listed after the position.
func (p *pagePosition) before(op *dcctlv1.Operation) bool {
	ct := op.GetCreateTime().AsTime()

	return ct.Before(p.createTime) || ct.Equal(p.createTime) && op.GetName() < p.name
}

// encodePageToken returns base64url of `<create_time in unix nanoseconds>|<name>`.
// The token is opaque to clients.
func encodePageToken(op *dcctlv1.Operation) string {
	raw := strconv.FormatInt(op.GetCreateTime().AsTime().UnixNano(), 10) + "|" + op.GetName()

	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodePageToken(token string) (*pagePosition, error) {
	if token == "" {
		return nil, nil //nolint:nilnil // no token is the first page, not an error
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, invalidPageToken()
	}

	nanos, name, ok := strings.Cut(string(raw), "|")
	if !ok || name == "" {
		return nil, invalidPageToken()
	}

	ns, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, invalidPageToken()
	}

	return &pagePosition{createTime: time.Unix(0, ns), name: name}, nil
}

func operationNotFound(name string) error {
	return apierr.Status(apierr.ReasonOperationNotFound, "operation not found",
		map[string]string{"operation": name}).Err()
}

func invalidPageToken() error {
	return apierr.New(apierr.ReasonRequestInvalid, "invalid request", &errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{
			{Field: "page_token", Description: "is not a token of this service"},
		},
	})
}
