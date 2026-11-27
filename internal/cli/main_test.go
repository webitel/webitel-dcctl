package cli

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// A cgo DNS lookup for localhost may finish after its test; its goroutine belongs to net.
	goleak.VerifyTestMain(m, goleak.IgnoreAnyFunction("net.cgoLookupIP"))
}
