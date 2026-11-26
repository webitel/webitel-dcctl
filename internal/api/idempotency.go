package api

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

// requestOneof is the oneof of Operation with the request that created it.
const requestOneof = "request"

// replay returns the operation the request_id id started, as the copy that
// answers for it holds it, and nil when no copy that was read holds one; the
// same id with another request fails with REQUEST_ID_MISMATCH. dcctl/op keeps
// the last operation of each KV, so an id is known while its operation is
// the last one in some KV.
//
// A read that fails answers nil: the operation then starts as if the id were
// new, and the engine refuses it if the copies hold an unfinished one.
func (s *actionServer) replay(ctx context.Context, id string, req proto.Message) (*dcctlv1.Operation, error) {
	view, err := s.ops.Read(ctx)
	if err != nil {
		s.log.WarnCtx(ctx, "copies of dcctl/op not read for the request_id", logging.Err(err))

		return nil, nil //nolint:nilnil // the id is treated as new
	}

	for _, c := range view.Copies {
		if c.Status != coord.StatusPresent || c.Operation.GetRequestId() != id {
			continue
		}

		op := view.Of(c.Operation.GetName()).Operation
		if !sameRequest(requestOf(op), req) {
			return nil, apierr.Status(apierr.ReasonRequestIDMismatch,
				"request_id was used by another request", map[string]string{"operation": op.GetName()}).Err()
		}

		return op, nil
	}

	return nil, nil //nolint:nilnil // no operation has the id
}

// storedRequest returns req as the operation stores it: without
// validate_only, without a confirmation that confirms nothing, and without
// unknown fields, which dcctl/op does not keep.
func storedRequest(req proto.Message) proto.Message {
	out := proto.Clone(req)
	m := out.ProtoReflect()
	fields := m.Descriptor().Fields()

	discardUnknown(m)

	if f := fields.ByName("validate_only"); f != nil {
		m.Clear(f)
	}

	if f := fields.ByName("confirmation"); f != nil && m.Has(f) && proto.Size(m.Get(f).Message().Interface()) == 0 {
		m.Clear(f)
	}

	return out
}

// discardUnknown clears the unknown fields of m and of the messages in it.
func discardUnknown(m protoreflect.Message) {
	m.SetUnknown(nil)
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case f.Message() == nil:
		case f.IsList():
			for i := range v.List().Len() {
				discardUnknown(v.List().Get(i).Message())
			}
		case f.IsMap():
			if f.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, e protoreflect.Value) bool {
					discardUnknown(e.Message())

					return true
				})
			}
		default:
			discardUnknown(v.Message())
		}

		return true
	})
}

// sameRequest reports whether stored, the request of an operation, is req.
func sameRequest(stored, req proto.Message) bool {
	return stored != nil && proto.Equal(storedRequest(stored), storedRequest(req))
}

// requestOf returns the request that created op, nil when it has none.
func requestOf(op *dcctlv1.Operation) proto.Message {
	m := op.ProtoReflect()

	f := m.WhichOneof(m.Descriptor().Oneofs().ByName(requestOneof))
	if f == nil {
		return nil
	}

	return m.Get(f).Message().Interface()
}

// setRequest sets req as the request of op; a request no field of the oneof
// takes is left out.
func setRequest(op *dcctlv1.Operation, req proto.Message) {
	if req == nil {
		return
	}

	m := op.ProtoReflect()
	fields := m.Descriptor().Oneofs().ByName(requestOneof).Fields()
	name := req.ProtoReflect().Descriptor().FullName()

	for i := range fields.Len() {
		if f := fields.Get(i); f.Message().FullName() == name {
			m.Set(f, protoreflect.ValueOfMessage(req.ProtoReflect()))

			return
		}
	}
}
