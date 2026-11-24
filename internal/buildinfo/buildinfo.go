// Package buildinfo identifies dcctl: its service name and build version.
package buildinfo

// ServiceName is the name of dcctl in Consul, where the peer listeners
// register and are looked up, and in OpenTelemetry.
const ServiceName = "webitel-dcctl"

// Version is set with -ldflags "-X github.com/webitel/webitel-dcctl/internal/buildinfo.Version=<version>".
var Version = "dev"
