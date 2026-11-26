package api

import (
	"context"
	"strings"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// DatacenterServer implements DatacenterService. Without actions, see
// [Options.Actions], the actions answer Unimplemented.
type DatacenterServer struct {
	dcctlv1.UnimplementedDatacenterServiceServer

	src     StatusSource
	address string
	log     *logging.Logger
	now     func() time.Time
	actions *actionServer
}

// NewDatacenterServer returns the DatacenterService of src. address is
// listen.peer of this instance.
func NewDatacenterServer(src StatusSource, address string, log *logging.Logger) *DatacenterServer {
	return &DatacenterServer{src: src, address: address, log: log, now: time.Now}
}

// ListDatacenters returns every datacenter in one page, whatever the page
// fields of the request are.
func (s *DatacenterServer) ListDatacenters(ctx context.Context, _ *dcctlv1.ListDatacentersRequest) (*dcctlv1.ListDatacentersResponse, error) {
	st, err := statusAt(s.src, s.now())
	if err != nil {
		return nil, err
	}

	m := newMapper(&st, s.address)
	out := &dcctlv1.ListDatacentersResponse{}

	for i := range st.Datacenters {
		out.Datacenters = append(out.Datacenters, m.datacenter(&st.Datacenters[i], false))
	}

	if m.err != nil {
		return nil, rpcError(ctx, s.log, "datacenters cannot be shown", m.err)
	}

	return out, nil
}

// GetDatacenter returns the details of a datacenter.
func (s *DatacenterServer) GetDatacenter(ctx context.Context, req *dcctlv1.GetDatacenterRequest) (*dcctlv1.Datacenter, error) {
	st, err := statusAt(s.src, s.now())
	if err != nil {
		return nil, err
	}

	dc := strings.TrimPrefix(req.GetName(), "datacenters/")

	for i := range st.Datacenters {
		if st.Datacenters[i].Name != dc {
			continue
		}

		m := newMapper(&st, s.address)

		out := m.datacenter(&st.Datacenters[i], true)
		if m.err != nil {
			return nil, rpcError(ctx, s.log, "datacenter cannot be shown", m.err)
		}

		return out, nil
	}

	return nil, datacenterNotFound(dc)
}
