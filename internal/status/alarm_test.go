package status_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func TestAlarmID_Type(t *testing.T) {
	ids := status.AlarmIDs()

	assert.Len(t, ids, len(dcctlv1.Status_Alarm_Type_name)-1)

	for i, id := range ids {
		assert.Equal(t, strings.ToUpper(string(id)), id.Type().String())
		assert.Equal(t, dcctlv1.Status_Alarm_Type(i+1), id.Type(), "AlarmIDs is in the order of the proto enum")
	}

	assert.Equal(t, dcctlv1.Status_Alarm_TYPE_UNSPECIFIED, status.AlarmID("unknown").Type())
}

func TestSeverity_Proto(t *testing.T) {
	assert.Equal(t, dcctlv1.Status_Alarm_CRITICAL, status.SeverityCritical.Proto())
	assert.Equal(t, dcctlv1.Status_Alarm_WARNING, status.SeverityWarning.Proto())
	assert.Equal(t, dcctlv1.Status_Alarm_SEVERITY_UNSPECIFIED, status.Severity("").Proto())
}
