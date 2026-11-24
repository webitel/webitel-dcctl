package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/config"
)

// Values of error.type in the failed login log.
const (
	errMalformed = "credentials_malformed"
	errInvalid   = "credentials_invalid"
)

// Basic authenticates calls on listen.http by the basic auth credentials in
// the authorization metadata.
type Basic struct {
	users map[string]user
	// dummy is compared for a name that is not a user, so the response time
	// does not tell which names exist.
	dummy []byte
	log   *logging.Logger
}

type user struct {
	hash []byte
	role config.Role
}

// NewBasic returns the authenticator of users. It refuses no users: nobody
// could log in, and an empty list must not mean "no authentication".
//
// Hashes of different bcrypt costs are logged as a warning: the dummy hash
// takes the highest cost, so a user with a lower one answers faster than a
// name that is not a user.
func NewBasic(ctx context.Context, users []config.User, log *logging.Logger) (*Basic, error) {
	if len(users) == 0 {
		return nil, errors.New("auth.users: no users")
	}

	b := &Basic{users: make(map[string]user, len(users)), log: log}
	minCost, maxCost := bcrypt.MaxCost, bcrypt.MinCost

	for i, u := range users {
		cost, err := bcrypt.Cost([]byte(u.PasswordHash))
		if err != nil {
			return nil, fmt.Errorf("auth.users[%d].password_hash: not a bcrypt hash", i)
		}

		minCost, maxCost = min(minCost, cost), max(maxCost, cost)
		b.users[u.Name] = user{hash: []byte(u.PasswordHash), role: u.Role}
	}

	if minCost != maxCost {
		log.WarnCtx(ctx, "auth.users password hashes have different bcrypt costs; "+
			"the response time may tell which user names exist",
			logging.Int("bcrypt.cost.min", minCost), logging.Int("bcrypt.cost.max", maxCost))
	}

	dummy, err := bcrypt.GenerateFromPassword([]byte(rand.Text()), maxCost)
	if err != nil {
		return nil, fmt.Errorf("dummy password hash: %w", err)
	}

	b.dummy = dummy

	return b, nil
}

// UnaryServerInterceptor authenticates the call; see StreamServerInterceptor.
func (b *Basic) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := b.authenticate(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor authenticates the call. The handler gets the user
// name in the context; the interceptor drops the authorization metadata,
// its gateway-prefixed copy and InitiatorKey. A call without valid
// credentials fails with AUTHENTICATION_REQUIRED; a failed login is logged
// with the client address.
func (b *Basic) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := b.authenticate(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}

		return handler(srv, serverStream{ServerStream: ss, ctx: ctx})
	}
}

func (b *Basic) authenticate(ctx context.Context, method string) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	values := md.Get(authorizationKey)
	if len(values) == 0 {
		return nil, errUnauthenticated()
	}

	name, password, ok := parseBasic(values)
	if !ok {
		b.logFailure(ctx, md, errMalformed, "")

		return nil, errUnauthenticated()
	}

	u, known := b.users[name]

	hash := b.dummy
	if known {
		hash = u.hash
	}

	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil || !known {
		if !known {
			// A name that is not a user may be a password typed into the
			// wrong field.
			name = ""
		}

		b.logFailure(ctx, md, errInvalid, name)

		return nil, errUnauthenticated()
	}

	if err := authorize(u.role, method); err != nil {
		return nil, err
	}

	md = md.Copy()
	delete(md, authorizationKey)
	delete(md, runtime.MetadataPrefix+authorizationKey)
	delete(md, InitiatorKey)

	ctx = metadata.NewIncomingContext(ctx, md)

	return context.WithValue(ctx, userKey{}, name), nil
}

// authorize checks that role may call method. Operator, the only role, may
// call every method.
func authorize(role config.Role, method string) error {
	if role == config.RoleOperator {
		return nil
	}

	return status.Errorf(codes.PermissionDenied, "role %q may not call %s", role, method)
}

func errUnauthenticated() error {
	return apierr.New(apierr.ReasonAuthenticationRequired, "authentication required")
}

// parseBasic reads the one Basic credentials value; the scheme is case-insensitive.
func parseBasic(values []string) (name, password string, ok bool) {
	if len(values) != 1 {
		return "", "", false
	}

	const prefix = "basic "

	v := values[0]
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(v[len(prefix):])
	if err != nil {
		return "", "", false
	}

	return strings.Cut(string(decoded), ":")
}

// logFailure logs a failed login without the credentials. name is logged
// only when it is a user.
func (b *Basic) logFailure(ctx context.Context, md metadata.MD, errType, name string) {
	fields := []logging.Field{
		logging.KV(semconv.ClientAddress(clientAddress(ctx, md)), semconv.ErrorTypeKey.String(errType)),
	}
	if name != "" {
		fields = append(fields, logging.KV(semconv.UserName(name)))
	}

	b.log.WarnCtx(ctx, "login failed", fields...)
}

// clientAddress returns the address of a direct gRPC client from its
// connection. A gateway call comes through an in-memory connection, so its
// address is in x-forwarded-for: the gateway appends the address of the HTTP
// client, which behind nginx is nginx itself, and nginx appends the address
// it got the request from. The values left of those come from the client.
func clientAddress(ctx context.Context, md metadata.MD) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}

	if strings.HasPrefix(p.Addr.Network(), "tcp") {
		if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
			return host
		}

		return p.Addr.String()
	}

	var hops []string

	for _, v := range md.Get("x-forwarded-for") {
		for hop := range strings.SplitSeq(v, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}

	switch len(hops) {
	case 0:
		return p.Addr.String()
	case 1:
		return hops[0]
	default:
		return hops[len(hops)-2]
	}
}
