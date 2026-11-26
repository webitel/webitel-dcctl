package api

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/operation"
)

// replay returns the operation the request_id id started, see
// [operation.Replay], from the copies of dcctl/op.
//
// A read that fails answers nil: the operation then starts as if the id were
// new, and the engine checks the id again under the lock.
func (s *actionServer) replay(ctx context.Context, id string, req proto.Message) (*dcctlv1.Operation, error) {
	view, err := s.ops.Read(ctx)
	if err != nil {
		s.log.WarnCtx(ctx, "copies of dcctl/op not read for the request_id", logging.Err(err))

		return nil, nil //nolint:nilnil // the id is treated as new
	}

	return operation.Replay(view, id, req)
}

// setRequest sets req as the request of op; a request no field of the oneof
// takes is left out.
func setRequest(op *dcctlv1.Operation, req proto.Message) {
	if req == nil {
		return
	}

	m := op.ProtoReflect()
	fields := m.Descriptor().Oneofs().ByName("request").Fields()
	name := req.ProtoReflect().Descriptor().FullName()

	for i := range fields.Len() {
		if f := fields.Get(i); f.Message().FullName() == name {
			m.Set(f, protoreflect.ValueOfMessage(req.ProtoReflect()))

			return
		}
	}
}
