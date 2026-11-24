// Package auth authenticates dcctl calls: basic auth of people and external
// initiators on listen.http, the node certificates of other dcctl instances
// on listen.peer, and the HTTP rules of the gateway.
package auth

import (
	"context"

	"google.golang.org/grpc"
)

// InitiatorKey is the metadata with who started an action and where, set by
// the instance that forwards the call. Only listen.peer accepts it.
const InitiatorKey = "x-dcctl-initiator"

// authorizationKey is the metadata the gateway puts the Authorization header in.
const authorizationKey = "authorization"

type (
	userKey      struct{}
	initiatorKey struct{}
)

// UserFromContext returns the name of the user authenticated by basic auth.
func UserFromContext(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(userKey{}).(string)

	return name, ok
}

// InitiatorFromContext returns the InitiatorKey metadata of a call from
// another dcctl instance.
func InitiatorFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(initiatorKey{}).(string)

	return v, ok
}

// serverStream replaces the context of a stream.
type serverStream struct {
	grpc.ServerStream

	ctx context.Context //nolint:containedctx // grpc.ServerStream returns its context from a method
}

func (s serverStream) Context() context.Context { return s.ctx }
