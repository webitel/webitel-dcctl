// Package api implements the gRPC services of dcctl that the UI, the CLI and
// external systems use, and serves the UI.
//
// Reads answer with the view of the instance that receives them, named in
// observer; they are never forwarded to another instance. Status and
// datacenters come from the last collection cycle, operations from the copies
// of dcctl/op read on every request, and the journal from the KV of every
// datacenter that answers.
package api
