// Package arbiter is the third voter of warm_standby: the ArbiterService
// server over the record store, the daemon that serves it, and the client
// dcctl instances call it with.
package arbiter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/arbiter/store"
)

const recordPrefix = "records/"

// Server serves ArbiterService over a store.
type Server struct {
	dcctlv1.UnimplementedArbiterServiceServer

	store *store.Store
	log   *logging.Logger
}

// NewServer returns the service over st.
func NewServer(st *store.Store, log *logging.Logger) *Server {
	return &Server{store: st, log: log}
}

// GetRecord returns a record.
func (s *Server) GetRecord(ctx context.Context, req *dcctlv1.GetRecordRequest) (*dcctlv1.Record, error) {
	name := recordName(req.GetName())

	rec, err := s.store.Get(name)
	if err != nil {
		return nil, s.status(ctx, name, "name", err)
	}

	return toProto(name, rec), nil
}

// UpdateRecord writes a record if its etag matches.
func (s *Server) UpdateRecord(ctx context.Context, req *dcctlv1.UpdateRecordRequest) (*dcctlv1.Record, error) {
	r := req.GetRecord()
	name := recordName(r.GetName())

	index, err := parseEtag(r.GetEtag(), req.GetAllowMissing())
	if err != nil {
		return nil, err
	}

	rec, err := s.store.CompareAndSwap(name, index, r.GetValue())
	if err != nil {
		return nil, s.status(ctx, name, "record.name", err)
	}

	return toProto(name, rec), nil
}

// recordName returns the store name of a resource name, or "" for a name
// outside records/, which the store rejects as unknown.
func recordName(resource string) string {
	name, ok := strings.CutPrefix(resource, recordPrefix)
	if !ok {
		return ""
	}

	return name
}

// parseEtag returns the store index of etag; 0 creates the record. Index 0
// is never a stored index, so a caller cannot create a record by sending
// etag "0" without allow_missing.
func parseEtag(etag string, allowMissing bool) (uint64, error) {
	if allowMissing {
		if etag != "" {
			return 0, invalidField("record.etag", "etag must be empty with allow_missing")
		}

		return 0, nil
	}

	if etag == "" {
		return 0, invalidField("record.etag", "etag is required unless allow_missing is set")
	}

	index, err := strconv.ParseUint(etag, 10, 64)
	if err != nil || index == 0 {
		return 0, invalidField("record.etag", "etag is not a record index")
	}

	return index, nil
}

func invalidField(field, msg string) error {
	return apierr.New(apierr.ReasonRequestInvalid, "invalid request", &errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: msg}},
	})
}

// status maps a store error of record name, sent in the request field
// nameField, to the API. Internal errors are logged and answered without
// their text, which holds the store path.
func (s *Server) status(ctx context.Context, name, nameField string, err error) error {
	if mismatch, ok := errors.AsType[*store.MismatchError](err); ok {
		return apierr.New(apierr.ReasonEtagMismatch, "etag does not match the record",
			protoadapt.MessageV1Of(toProto(name, mismatch.Current)))
	}

	switch {
	case errors.Is(err, store.ErrUninitialized):
		return apierr.New(apierr.ReasonArbiterUninitialized, "arbiter is not initialized")
	case errors.Is(err, store.ErrNotFound):
		return apierr.New(apierr.ReasonRecordNotFound, fmt.Sprintf("record %s%s not found", recordPrefix, name))
	case errors.Is(err, store.ErrUnknownRecord):
		return invalidField(nameField, "unknown record")
	case errors.Is(err, store.ErrClosed):
		return status.Error(codes.Unavailable, "arbiter store is not available")
	}

	s.log.ErrorCtx(ctx, "arbiter store failed", logging.Str("record", name), logging.Err(err))

	return status.Error(codes.Internal, "arbiter store failed")
}

func toProto(name string, rec store.Record) *dcctlv1.Record {
	return &dcctlv1.Record{
		Name:       recordPrefix + name,
		Value:      rec.Value,
		Etag:       strconv.FormatUint(rec.Index, 10),
		UpdateTime: timestamppb.New(rec.UpdateTime),
	}
}
