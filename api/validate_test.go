package api_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/assert"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

func TestUpdateRecordRequestValidation(t *testing.T) {
	t.Parallel()

	record := func(etag string) *dcctlv1.Record {
		return &dcctlv1.Record{Name: "records/state", Value: []byte(`{"generation":1}`), Etag: etag}
	}

	tests := []struct {
		name    string
		req     *dcctlv1.UpdateRecordRequest
		wantErr bool
	}{
		{"update with etag", &dcctlv1.UpdateRecordRequest{Record: record("1763895300000000000")}, false},
		{"create with allow_missing", &dcctlv1.UpdateRecordRequest{Record: record(""), AllowMissing: true}, false},
		{"no record", &dcctlv1.UpdateRecordRequest{AllowMissing: true}, true},
		{"empty etag without allow_missing", &dcctlv1.UpdateRecordRequest{Record: record("")}, true},
		{"etag not a number", &dcctlv1.UpdateRecordRequest{Record: record("12a")}, true},
		{"etag longer than uint64", &dcctlv1.UpdateRecordRequest{Record: record("123456789012345678901")}, true},
		{"unknown record", &dcctlv1.UpdateRecordRequest{Record: &dcctlv1.Record{Name: "records/lock", Value: []byte("{}"), Etag: "1"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := protovalidate.Validate(tt.req)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
