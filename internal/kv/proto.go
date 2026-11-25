package kv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var (
	encodeOptions = protojson.MarshalOptions{UseProtoNames: true}
	decodeOptions = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// EncodeProto returns the protojson of m as dcctl stores it, with proto field names.
func EncodeProto(m proto.Message) ([]byte, error) {
	return encodeOptions.Marshal(m)
}

// DecodeProto reads the protojson b into m. Fields and enum values m does not know are
// ignored, so an older dcctl reads what a newer one wrote; an unknown enum value reads as
// the zero value.
func DecodeProto(b []byte, m proto.Message) error {
	return decodeOptions.Unmarshal(b, m)
}

// PreserveUnknown returns cur, the protojson of a message of desc, with what prev holds
// that desc does not know, so that rewriting a record keeps what a newer dcctl added:
// unknown fields, and unknown values of enum fields that cur leaves unset. It looks into
// nested messages, and into the items of lists of the messages in [listIdentity], matched
// by their identity fields; items of other lists, maps and google.protobuf messages are
// taken from cur as they are. A field set in cur is never replaced. cur is returned
// unchanged when prev has nothing to add.
//
// A oneof is assumed to be set once and never changed: a newer member that prev holds
// is kept next to the member cur sets, and a record with two members of one oneof does
// not decode.
func PreserveUnknown(desc protoreflect.MessageDescriptor, prev, cur []byte) ([]byte, error) {
	if len(prev) == 0 {
		return cur, nil
	}

	p, err := decodeObject(prev)
	if err != nil {
		return nil, fmt.Errorf("previous record: %w", err)
	}

	c, err := decodeObject(cur)
	if err != nil {
		return nil, fmt.Errorf("new record: %w", err)
	}

	if !mergeUnknown(desc, p, c) {
		return cur, nil
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("encode merged record: %w", err)
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// decodeObject decodes a JSON object keeping numbers as written.
func decodeObject(b []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()

	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}

	if m == nil {
		return nil, errors.New("not a JSON object")
	}

	return m, nil
}

// listIdentity names the fields that identify an item of a list of the message, so
// that the item of prev and the item of cur are matched.
var listIdentity = map[protoreflect.FullName][]protoreflect.Name{
	"webitel.dcctl.v1.Operation.Step": {"index", "type", "stage", "datacenter"},
	"webitel.dcctl.v1.Check":          {"id", "datacenter"},
}

func mergeUnknown(desc protoreflect.MessageDescriptor, prev, cur map[string]any) bool {
	changed := false

	for key, pv := range prev {
		fd := fieldOf(desc, key)
		cv, inCur := cur[key]

		switch {
		case fd == nil:
			if !inCur {
				cur[key] = pv
				changed = true
			}
		case fd.Enum() != nil && !fd.IsList() && !fd.IsMap():
			if !inCur && !knownEnum(fd.Enum(), pv) {
				cur[key] = pv
				changed = true
			}
		case !inCur, fd.Message() == nil, fd.IsMap(), fd.Message().FullName().Parent() == "google.protobuf":
		case fd.IsList():
			changed = mergeList(fd.Message(), pv, cv) || changed
		default:
			changed = mergeValue(fd.Message(), pv, cv) || changed
		}
	}

	return changed
}

func mergeList(desc protoreflect.MessageDescriptor, prev, cur any) bool {
	fields, ok := listIdentity[desc.FullName()]
	if !ok {
		return false
	}

	p, ok := prev.([]any)
	if !ok {
		return false
	}

	c, ok := cur.([]any)
	if !ok {
		return false
	}

	byID := itemsByIdentity(desc, fields, p)
	curIDs := itemsByIdentity(desc, fields, c)
	changed := false

	for id, ci := range curIDs {
		if pi, ok := byID[id]; ok {
			changed = mergeValue(desc, p[pi], c[ci]) || changed
		}
	}

	return changed
}

// itemsByIdentity returns the index of every item of list by its identity. Items whose
// identity is not unique are left out: they cannot be matched.
func itemsByIdentity(desc protoreflect.MessageDescriptor, fields []protoreflect.Name, list []any) map[string]int {
	out := make(map[string]int, len(list))
	dup := make(map[string]bool)

	for i, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}

		id := identity(desc, fields, obj)

		if _, seen := out[id]; seen {
			dup[id] = true
		}

		out[id] = i
	}

	for id := range dup {
		delete(out, id)
	}

	return out
}

// identity returns the values of fields in obj as protojson reads them: an unset field
// and an enum value desc does not know are the zero value.
func identity(desc protoreflect.MessageDescriptor, fields []protoreflect.Name, obj map[string]any) string {
	var b strings.Builder

	for _, name := range fields {
		fd := desc.Fields().ByName(name)
		if fd == nil {
			continue
		}

		v, ok := obj[string(fd.Name())]
		if !ok {
			v, ok = obj[fd.JSONName()]
		}

		b.WriteString(scalar(fd, v, ok))
		b.WriteByte(0)
	}

	return b.String()
}

func scalar(fd protoreflect.FieldDescriptor, v any, set bool) string {
	if fd.Enum() != nil {
		if set && knownEnum(fd.Enum(), v) {
			return enumName(fd.Enum(), v)
		}

		return string(fd.Enum().Values().ByNumber(0).Name())
	}

	if !set {
		return fd.Default().String()
	}

	return fmt.Sprint(v)
}

func knownEnum(e protoreflect.EnumDescriptor, v any) bool {
	return enumName(e, v) != ""
}

// enumName returns the name of the value v of e, written as protojson writes it, or ""
// for a value e does not know.
func enumName(e protoreflect.EnumDescriptor, v any) string {
	switch v := v.(type) {
	case string:
		if e.Values().ByName(protoreflect.Name(v)) != nil {
			return v
		}
	case json.Number:
		n, err := strconv.ParseInt(v.String(), 10, 32)
		if err != nil {
			return ""
		}

		if ev := e.Values().ByNumber(protoreflect.EnumNumber(n)); ev != nil {
			return string(ev.Name())
		}
	}

	return ""
}

func mergeValue(desc protoreflect.MessageDescriptor, prev, cur any) bool {
	p, ok := prev.(map[string]any)
	if !ok {
		return false
	}

	c, ok := cur.(map[string]any)
	if !ok {
		return false
	}

	return mergeUnknown(desc, p, c)
}

// fieldOf returns the field of desc that protojson reads from key: by its JSON name or
// its proto name.
func fieldOf(desc protoreflect.MessageDescriptor, key string) protoreflect.FieldDescriptor {
	if fd := desc.Fields().ByJSONName(key); fd != nil {
		return fd
	}

	return desc.Fields().ByName(protoreflect.Name(key))
}
