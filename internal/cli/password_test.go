package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func TestPasswordIsNeverPrinted(t *testing.T) {
	cases := map[string]func(e *env) []string{
		"auth failure": func(e *env) []string {
			e.api.status = func(*dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
				return nil, apierr.New(apierr.ReasonAuthenticationRequired, "bad")
			}

			return []string{"status"}
		},
		"auth failure json": func(e *env) []string {
			e.api.status = func(*dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
				return nil, status.Error(codes.Unauthenticated, "nope")
			}

			return []string{"status", "-o", "json"}
		},
		"refusal json": func(e *env) []string {
			e.script(failWith(refusal(apierr.ReasonPreflightFailed, "x",
				testCheck("a.b", dcctlv1.Check_FAIL, dcctlv1.Check_BLOCK, "d"))))

			return []string{"switch", "dc_b", "--yes", "-o", "json"}
		},
		"usage error": func(*env) []string { return []string{"switch", "--nope"} },
		"unreachable": func(e *env) []string {
			e.t.Setenv("DCCTL_SERVER", "127.0.0.1:1")

			return []string{"status"}
		},
		"plaintext refused": func(e *env) []string {
			e.t.Setenv("DCCTL_SERVER", "10.0.0.1:10022")

			return []string{"status"}
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			res := e.run(setup(e)...)

			assert.NotEqual(t, ExitOK, res.code)
			assert.NotContains(t, res.stdout+res.stderr, testPassword)
		})
	}
}
