// Package buildinfo holds build metadata set with -ldflags at build time.
package buildinfo

// Version is set with -ldflags "-X github.com/webitel/webitel-dcctl/internal/buildinfo.Version=<version>".
var Version = "dev"
