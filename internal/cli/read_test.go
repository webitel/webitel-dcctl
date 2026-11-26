package cli

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func TestStatus(t *testing.T) {
	e := newEnv(t)
	e.api.status = func(*dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) { return testStatus(), nil }

	res := e.run("status")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "status_table", res.stdout)

	res = e.run("status", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "status_json", res.stdout)
}

func TestStatusOfDatacenter(t *testing.T) {
	e := newEnv(t)
	e.api.getDatacenter = func(r *dcctlv1.GetDatacenterRequest) (*dcctlv1.Datacenter, error) {
		return testDatacenter(shortName(r.GetName()), dcctlv1.Datacenter_STANDBY), nil
	}

	res := e.run("status", "dc_b")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "status_dc_table", res.stdout)

	res = e.run("status", "datacenters/dc_b", "--output", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "status_dc_json", res.stdout)

	reqs := e.api.requests("/webitel.dcctl.v1.DatacenterService/GetDatacenter")
	require.Len(t, reqs, 2)

	for _, r := range reqs {
		assert.Equal(t, "datacenters/dc_b", r.(*dcctlv1.GetDatacenterRequest).GetName())
	}
}

func TestStatusUsage(t *testing.T) {
	e := newEnv(t)

	for _, args := range [][]string{
		{"status", "a", "b"},
		{"status", "dc_b/../x"},
		{"status", "-o", "yaml"},
		{"datacenters", "dc_a"},
		{"verify", "x"},
		{"ops", "nope"},
		{"ops", "get"},
		{"ops", "get", "a b"},
		{"ops", "--limit", "0"},
		{"journal", "x"},
		{"journal", "--type", "bogus"},
		{"journal", "--type", "TYPE_UNSPECIFIED"},
		{"journal", "--dc", `x" OR type = "ALARM`},
		{"journal", "--op", "a b"},
		{"journal", "--limit", "-1"},
	} {
		res := e.run(args...)

		assert.Equal(t, ExitUsage, res.code, "%v: %s", args, res.stderr)
	}

	assert.Empty(t, e.api.methods(), "a usage error does not reach the server")
}

func TestDatacenters(t *testing.T) {
	e := newEnv(t)
	e.api.listDatacenters = func(*dcctlv1.ListDatacentersRequest) (*dcctlv1.ListDatacentersResponse, error) {
		return &dcctlv1.ListDatacentersResponse{Datacenters: testStatus().GetDatacenters()}, nil
	}

	res := e.run("datacenters")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "datacenters_table", res.stdout)

	res = e.run("dcs", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "datacenters_json", res.stdout)
}

func TestVerify(t *testing.T) {
	e := newEnv(t)
	checks := make([]*dcctlv1.Check, 0, 2)
	checks = append(checks, testCheck("verify.state", dcctlv1.Check_PASS, dcctlv1.Check_EFFECT_UNSPECIFIED, "state is in order"))
	e.api.verify = func(*dcctlv1.VerifyStatusRequest) (*dcctlv1.VerifyStatusResponse, error) {
		return &dcctlv1.VerifyStatusResponse{Checks: checks}, nil
	}

	res := e.run("verify")
	assert.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "verify_table", res.stdout)

	checks = append(checks, testCheck("verify.jobs", dcctlv1.Check_FAIL, dcctlv1.Check_EFFECT_UNSPECIFIED, "a job is down"))

	res = e.run("verify", "-o", "json")
	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stdout, `"verify.jobs"`)
}

func TestOperationsList(t *testing.T) {
	e := newEnv(t)
	all := []*dcctlv1.Operation{
		testOperation("op3", dcctlv1.Operation_RUNNING),
		testOperation("op2", dcctlv1.Operation_SUCCEEDED),
		testOperation("op1", dcctlv1.Operation_FAILED),
	}
	e.api.listOperations = func(r *dcctlv1.ListOperationsRequest) (*dcctlv1.ListOperationsResponse, error) {
		start := 0
		if r.GetPageToken() != "" {
			_, _ = fmt.Sscan(r.GetPageToken(), &start)
		}

		end := min(start+int(r.GetPageSize()), len(all), start+2)
		resp := &dcctlv1.ListOperationsResponse{Operations: all[start:end]}

		if end < len(all) {
			resp.NextPageToken = fmt.Sprint(end)
		}

		return resp, nil
	}

	res := e.run("ops")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "ops_table", res.stdout)

	res = e.run("operations", "--limit", "3", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "ops_json", res.stdout)

	res = e.run("ops", "--limit", "1")
	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.Contains(t, res.stdout, "op3")
	assert.NotContains(t, res.stdout, "op2")
}

func TestOperationsGet(t *testing.T) {
	e := newEnv(t)
	e.api.getOperation = func(r *dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) {
		op := testOperation(shortName(r.GetName()), dcctlv1.Operation_FAILED)
		op.Error = &dcctlv1.OperationError{Reason: "STEP_FAILED", Detail: "patroni did not answer"}
		op.Warnings = []string{"dc_c was not repointed"}
		op.Checks = []*dcctlv1.Check{testCheck("coord.lock_free", dcctlv1.Check_PASS, dcctlv1.Check_BLOCK, "the lock is free")}

		return op, nil
	}

	res := e.run("ops", "get", "operations/op7")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "ops_get_table", res.stdout)

	res = e.run("ops", "get", "op7", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "ops_get_json", res.stdout)

	reqs := e.api.requests("/webitel.dcctl.v1.OperationService/GetOperation")
	require.Len(t, reqs, 2)
	assert.Equal(t, "operations/op7", reqs[0].(*dcctlv1.GetOperationRequest).GetName())
}

func TestJournal(t *testing.T) {
	e := newEnv(t)

	entry := func(n int) *dcctlv1.JournalEntry {
		return &dcctlv1.JournalEntry{
			Name: fmt.Sprintf("journalEntries/%d", n), CreateTime: ts(0), Type: dcctlv1.JournalEntry_ALARM,
			Result: dcctlv1.JournalEntry_WARNING, Datacenter: "datacenters/dc_b", Detail: fmt.Sprintf("entry %d", n),
		}
	}

	e.api.journal = func(r *dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error) {
		if r.GetPageToken() == "" {
			return &dcctlv1.ListJournalEntriesResponse{
				JournalEntries: []*dcctlv1.JournalEntry{entry(1), entry(2)}, NextPageToken: "next", Unreachable: []string{"dc_c"},
			}, nil
		}

		return &dcctlv1.ListJournalEntriesResponse{JournalEntries: []*dcctlv1.JournalEntry{entry(3)}}, nil
	}

	res := e.run("journal", "--type", "alarm", "--dc", "dc_b", "--op", "operations/op7", "--limit", "3")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "journal_table", res.stdout)
	assert.Contains(t, res.stderr, "may be incomplete: no answer from dc_c")

	reqs := e.api.requests("/webitel.dcctl.v1.JournalService/ListJournalEntries")
	require.Len(t, reqs, 2)

	first := reqs[0].(*dcctlv1.ListJournalEntriesRequest)
	assert.Equal(t, `type = "ALARM" AND datacenter = "datacenters/dc_b" AND operation = "operations/op7"`, first.GetFilter())
	assert.Equal(t, int32(3), first.GetPageSize())

	second := reqs[1].(*dcctlv1.ListJournalEntriesRequest)
	assert.Equal(t, "next", second.GetPageToken())
	assert.Equal(t, first.GetFilter(), second.GetFilter(), "a page token needs the filter it was made with")
	assert.Equal(t, int32(1), second.GetPageSize())

	res = e.run("journal", "--limit", "2", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "journal_json", res.stdout)
}

func TestRefusalsOfReads(t *testing.T) {
	e := newEnv(t)
	e.api.getDatacenter = func(*dcctlv1.GetDatacenterRequest) (*dcctlv1.Datacenter, error) {
		return nil, apierr.Status(apierr.ReasonDatacenterNotFound, "datacenter dc_x is not in the config",
			map[string]string{"datacenter": "datacenters/dc_x"}).Err()
	}

	res := e.run("status", "dc_x")
	assert.Equal(t, ExitFailed, res.code)
	assert.Equal(t, "error: datacenter dc_x is not in the config [DATACENTER_NOT_FOUND]\n", res.stderr)

	res = e.run("status", "dc_x", "-o", "json")
	assert.Equal(t, ExitFailed, res.code)
	golden(t, "error_json", res.stderr)

	e.api.status = func(*dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
		return nil, apierr.New(apierr.ReasonAuthenticationRequired, "invalid credentials")
	}

	res = e.run("status")
	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "authentication failed: the user or the password is not accepted")
	assert.NotContains(t, res.stderr, testPassword)

	e.api.status = nil

	res = e.run("status")
	assert.Equal(t, ExitFailed, res.code)
}

func TestJSONMatchesProto(t *testing.T) {
	// -o json prints the proto field names, which the HTTP API uses as well.
	b, err := marshalJSON(&dcctlv1.Confirmation{NotMaxLsn: true, MaxDiscardBytes: 5}, false)
	require.NoError(t, err)
	assert.JSONEq(t, `{"not_max_lsn":true,"max_discard_bytes":"5"}`, string(b))

	var _ proto.Message = (*dcctlv1.Confirmation)(nil)
}

func TestFlagsAmongArguments(t *testing.T) {
	e := newEnv(t)
	e.api.getDatacenter = func(r *dcctlv1.GetDatacenterRequest) (*dcctlv1.Datacenter, error) {
		return testDatacenter(shortName(r.GetName()), dcctlv1.Datacenter_STANDBY), nil
	}

	for _, args := range [][]string{
		{"status", "-o", "json", "dc_b"},
		{"status", "dc_b", "-o", "json"},
		{"status", "dc_b", "--output=json"},
		{"status", "--", "dc_b", "-o"},
	} {
		res := e.run(args...)

		if args[len(args)-1] == "-o" {
			assert.Equal(t, ExitUsage, res.code, args)

			continue
		}

		assert.Equal(t, ExitOK, res.code, "%v: %s", args, res.stderr)
		assert.True(t, strings.HasPrefix(res.stdout, "{"), args)
	}
}

func TestHelp(t *testing.T) {
	e := newEnv(t)

	res := e.run("journal", "--help")
	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.Contains(t, res.stdout, "--password-file")
	assert.Contains(t, res.stdout, "--limit")
	assert.NotContains(t, res.stdout, "--password ")
	assert.Empty(t, e.api.methods())
}

// A server that repeats a token or sends an empty page does not keep the
// command in a loop.
func TestPaginationEnds(t *testing.T) {
	for name, page := range map[string]*dcctlv1.ListOperationsResponse{
		"same token": {Operations: []*dcctlv1.Operation{testOperation("op1", dcctlv1.Operation_RUNNING)}, NextPageToken: "t"},
		"empty page": {NextPageToken: "t"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.api.listOperations = func(r *dcctlv1.ListOperationsRequest) (*dcctlv1.ListOperationsResponse, error) {
				return page, nil
			}

			res := e.run("ops", "--limit", "100")

			assert.Equal(t, ExitOK, res.code, res.stderr)
			assert.LessOrEqual(t, len(e.api.methods()), 2)
		})
	}

	e := newEnv(t)
	e.api.journal = func(*dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error) {
		return &dcctlv1.ListJournalEntriesResponse{NextPageToken: "t"}, nil
	}

	assert.Equal(t, ExitOK, e.run("journal", "--limit", "100").code)
	assert.Len(t, e.api.methods(), 1)
}

func TestLimitIsCheckedBeforeThePassword(t *testing.T) {
	e := newEnv(t)
	t.Setenv(passwordEnv, "")

	asked := false
	e.opts.ReadPassword = func(string, io.Writer) (string, error) {
		asked = true

		return "", nil
	}

	for _, args := range [][]string{{"ops", "--limit", "0"}, {"journal", "--limit", "0"}} {
		assert.Equal(t, ExitUsage, e.run(args...).code)
	}

	assert.False(t, asked)
}
