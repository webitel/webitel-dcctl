package operation

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

// requestOneof is the oneof of Operation with the request that created it.
const requestOneof = "request"

// Replay returns the operation the request_id id started, as the copy of view
// that answers for it holds it, and nil when no copy holds one; one started
// by another request than req fails with REQUEST_ID_MISMATCH. dcctl/op keeps
// the last operation of each KV, so an id is known while its operation is the
// last one in some KV.
func Replay(view coord.OpView, id string, req proto.Message) (*dcctlv1.Operation, error) {
	if id == "" {
		return nil, nil //nolint:nilnil // no id, nothing to replay
	}

	for _, c := range view.Copies {
		if c.Status != coord.StatusPresent || c.Operation.GetRequestId() != id {
			continue
		}

		op := view.Of(c.Operation.GetName()).Operation
		if !SameRequest(RequestOf(op), req) {
			return nil, apierr.Status(apierr.ReasonRequestIDMismatch,
				"request_id was used by another request", map[string]string{"operation": op.GetName()}).Err()
		}

		return op, nil
	}

	return nil, nil //nolint:nilnil // no operation has the id
}

// StoredRequest returns req as an operation stores it: without validate_only,
// without a confirmation that confirms nothing, and without unknown fields,
// which dcctl/op does not keep.
func StoredRequest(req proto.Message) proto.Message {
	if req == nil {
		return nil
	}

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

// SameRequest reports whether stored, the request of an operation, is req.
func SameRequest(stored, req proto.Message) bool {
	return stored != nil && req != nil && proto.Equal(StoredRequest(stored), StoredRequest(req))
}

// RequestOf returns the request that created op, nil when it has none.
func RequestOf(op *dcctlv1.Operation) proto.Message {
	m := op.ProtoReflect()

	f := m.WhichOneof(m.Descriptor().Oneofs().ByName(requestOneof))
	if f == nil {
		return nil
	}

	return m.Get(f).Message().Interface()
}
