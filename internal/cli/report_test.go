package cli

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// An error that wraps a status keeps its own text, as the daemon commands
// return them: the prefix tells which part failed.
func TestReportKeepsThePrefixOfAWrappedError(t *testing.T) {
	var out bytes.Buffer

	wrapped := fmt.Errorf("arbiter: %w", apierr.New(apierr.ReasonRecordNotFound, "record not found"))

	assert.Equal(t, ExitFailed, Report(&out, wrapped))
	assert.Contains(t, out.String(), "arbiter: ")

	out.Reset()

	hint := withHint(apierr.New(apierr.ReasonLockBusy, "busy"), "try later")

	assert.Equal(t, ExitFailed, Report(&out, hint))
	assert.Contains(t, out.String(), "busy [LOCK_BUSY]")
	assert.Contains(t, out.String(), "try later")
}
