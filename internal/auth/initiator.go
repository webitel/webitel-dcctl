package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"unicode"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

const (
	// maxInitiatorLen bounds the InitiatorKey value.
	maxInitiatorLen = 2048
	// maxInitiatorField bounds each text field of the initiator.
	maxInitiatorField = 256
	datacenterPrefix  = "datacenters/"
)

// Initiator returns who started the call and whether another dcctl instance
// forwarded it, valid for the journal and for forwarding. A user's call on
// listen.http names the user, the client address and self, the instance that
// accepted it; a client address that is not a short printable value is
// replaced with the address of the connection. A call on listen.peer is
// forwarded: it names whom the forwarding instance put in InitiatorKey, and
// the address of that instance must be an IP of its certificate.
//
// A call with neither fails with AUTHENTICATION_REQUIRED, and an initiator
// that is not valid with REQUEST_INVALID: the action would otherwise run on
// behalf of nobody.
func Initiator(ctx context.Context, self *dcctlv1.Instance) (ini *dcctlv1.Initiator, forwarded bool, err error) {
	if v, ok := InitiatorFromContext(ctx); ok {
		ini, err := decodeInitiator(v)
		if err == nil {
			err = certified(ctx, ini.GetInstance())
		}

		if err != nil {
			return nil, true, errInvalidInitiator()
		}

		return ini, true, nil
	}

	user, ok := UserFromContext(ctx)
	if !ok || user == "" {
		return nil, false, errUnauthenticated()
	}

	md, _ := metadata.FromIncomingContext(ctx)

	ini = &dcctlv1.Initiator{User: user, Address: clientAddress(ctx, md), Instance: proto.CloneOf(self)}
	if !text(ini.GetAddress(), false) {
		ini.Address = connAddress(ctx)
	}

	if err := validInitiator(ini); err != nil {
		return nil, false, errInvalidInitiator()
	}

	return ini, false, nil
}

// Forwarding returns ctx for the call that forwards an action of ini to
// another instance. Its outgoing metadata is InitiatorKey alone: nothing the
// client sent, its credentials included, goes further. An initiator that is
// not valid fails with REQUEST_INVALID.
func Forwarding(ctx context.Context, ini *dcctlv1.Initiator) (context.Context, error) {
	if err := validInitiator(ini); err != nil {
		return nil, errInvalidInitiator()
	}

	raw, err := proto.Marshal(ini)
	if err != nil {
		return nil, err
	}

	v := base64.RawURLEncoding.EncodeToString(raw)
	if len(v) > maxInitiatorLen {
		return nil, errInvalidInitiator()
	}

	return metadata.NewOutgoingContext(ctx, metadata.Pairs(InitiatorKey, v)), nil
}

func errInvalidInitiator() error {
	return apierr.New(apierr.ReasonRequestInvalid, "the initiator of the call is not valid")
}

// certified checks that the address of inst, the instance that forwarded the
// call, is an IP of the client certificate of the call.
func certified(ctx context.Context, inst *dcctlv1.Instance) error {
	_, leaf, ok := clientCert(ctx)
	if !ok {
		return errors.New("forwarded call without a client certificate")
	}

	host, _, err := net.SplitHostPort(inst.GetAddress())
	if err != nil {
		return err
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}

	if !slices.Contains(certIPs(leaf), ip.Unmap()) {
		return errors.New("forwarding instance address is not in its certificate")
	}

	return nil
}

// connAddress returns the host of the connection of the call, or "" when it
// is not a short printable value.
func connAddress(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}

	addr := p.Addr.String()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}

	if !text(addr, false) {
		return ""
	}

	return addr
}

func decodeInitiator(v string) (*dcctlv1.Initiator, error) {
	if len(v) > maxInitiatorLen {
		return nil, errors.New("initiator is too long")
	}

	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}

	ini := &dcctlv1.Initiator{}
	if err := proto.Unmarshal(raw, ini); err != nil {
		return nil, err
	}

	if err := validInitiator(ini); err != nil {
		return nil, err
	}

	return ini, nil
}

// validInitiator accepts a user's initiator with the instance that accepted
// the call.
func validInitiator(ini *dcctlv1.Initiator) error {
	inst := ini.GetInstance()

	switch {
	case ini.GetSystem():
		return errors.New("a forwarded call is not a system action")
	case !text(ini.GetUser(), true) || !text(ini.GetAddress(), false):
		return errors.New("initiator user or address is not valid")
	case !text(inst.GetNode(), true) || !text(inst.GetAddress(), false):
		return errors.New("initiator instance is not valid")
	case !strings.HasPrefix(inst.GetDatacenter(), datacenterPrefix) || !text(strings.TrimPrefix(inst.GetDatacenter(), datacenterPrefix), true):
		return errors.New("initiator datacenter is not valid")
	}

	return nil
}

// text reports whether s is a short printable value, and not empty when
// required.
func text(s string, required bool) bool {
	if s == "" {
		return !required
	}

	if len(s) > maxInitiatorField {
		return false
	}

	for _, r := range s {
		if r == unicode.ReplacementChar || !unicode.IsPrint(r) {
			return false
		}
	}

	return true
}
