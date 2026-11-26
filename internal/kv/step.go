package kv

import (
	"strings"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// The detail of a step of dcctl/op tells that the rollback of the step ran:
// it ends with [rolledBackMark] when the rollback succeeded, and has
// [rollbackFailedMark] and the error last when it failed. The record has no
// field for it, so dcctl writes and reads it only through [RollbackDetail]
// and [StepRolledBack].
const (
	rolledBackMark     = "rolled back"
	rollbackFailedMark = "rollback failed: "
	detailSeparator    = "; "
)

// RollbackDetail returns the detail of a step whose rollback ran after
// detail, the detail it had: rolled back when err is nil, failed with err
// otherwise.
func RollbackDetail(detail string, err error) string {
	mark := rolledBackMark
	if err != nil {
		mark = rollbackFailedMark + err.Error()
	}

	if detail == "" {
		return mark
	}

	return detail + detailSeparator + mark
}

// StepRolledBack reports whether the rollback of st ran, successfully or not,
// as [RollbackDetail] records it.
func StepRolledBack(st *dcctlv1.Operation_Step) bool {
	d := st.GetDetail()

	return d == rolledBackMark || strings.HasSuffix(d, detailSeparator+rolledBackMark) ||
		strings.HasPrefix(d, rollbackFailedMark) || strings.Contains(d, detailSeparator+rollbackFailedMark)
}
