package journal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

const (
	defaultPageSize = 50
	// filterHashLen is the length of the filter hash in a page token, in hex characters.
	filterHashLen = 16
)

// List returns a page of the journal, newest first, for
// JournalService.ListJournalEntries.
//
// It reads the journal of every KV that answers and merges the copies by
// key, so an entry that only one datacenter holds is listed, and the
// answer is the most complete one: it does not depend on which instance
// serves the request, nor on whether alignment has run. The datacenters
// whose KV does not answer are in unreachable, as their entries may be
// missing. It fails with KV_UNAVAILABLE only when no KV answers.
//
// The page token holds the key of the last entry returned and a hash of the
// filter; the next page continues below that key, so entries journaled or
// trimmed between pages do not move it. A token goes with the filter of the
// request that returned it, and page_size may change. An invalid filter or
// page_token is REQUEST_INVALID with BadRequest field violations. The limits
// of the request fields are the validation interceptor's.
func (j *Journal) List(ctx context.Context, req *dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error) {
	size, match, after, err := parseList(req)
	if err != nil {
		return nil, err
	}

	snaps := j.snapshots(ctx)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list journal: %w", err)
	}

	merged, unreachable, err := j.merge(ctx, snaps)
	if err != nil {
		return nil, err
	}

	resp := &dcctlv1.ListJournalEntriesResponse{}
	for _, dc := range unreachable {
		resp.Unreachable = append(resp.Unreachable, "datacenters/"+dc)
	}

	for _, c := range merged {
		if after != "" && c.suffix >= after {
			continue
		}

		e := new(dcctlv1.JournalEntry)
		if err := kv.DecodeProto(c.value, e); err != nil {
			j.log.WarnCtx(ctx, "journal entry does not decode", logging.Str("key", c.key), logging.Err(err))

			continue
		}

		e.Name = namePrefix + c.suffix

		if !match(e) {
			continue
		}

		if len(resp.GetJournalEntries()) == size {
			resp.NextPageToken = encodeToken(resp.GetJournalEntries()[size-1].GetName(), req.GetFilter())

			break
		}

		resp.JournalEntries = append(resp.JournalEntries, e)
	}

	return resp, nil
}

func parseList(req *dcctlv1.ListJournalEntriesRequest) (int, matcher, string, error) {
	size := int(req.GetPageSize())
	if size == 0 {
		size = defaultPageSize
	}

	match, err := compileFilter(req.GetFilter())
	if err != nil {
		var bad *filterError
		if errors.As(err, &bad) {
			return 0, nil, "", requestInvalid("filter", strings.TrimSpace(bad.Error()))
		}

		return 0, nil, "", err
	}

	after, err := decodeToken(req.GetPageToken(), req.GetFilter())
	if err != nil {
		return 0, nil, "", err
	}

	return size, match, after, nil
}

type candidate struct {
	key    string
	suffix string
	value  []byte
}

// merge returns the entries of all answering KVs by key, newest first, and
// the datacenters whose KV did not answer. The value of a key is the one of
// the first KV that holds it. The global Consul of stretch is not a
// datacenter and is never listed.
func (j *Journal) merge(ctx context.Context, snaps []snapshot) ([]candidate, []string, error) {
	var (
		out         []candidate
		unreachable []string
		answered    int
		errs        []error
	)

	seen := make(map[string]bool)

	for _, s := range snaps {
		if s.Err != nil {
			errs = append(errs, s.Err)
			if s.DC != coord.StretchName {
				unreachable = append(unreachable, s.DC)
			}

			continue
		}

		answered++

		for _, e := range s.Entries {
			if seen[e.Key] {
				continue
			}

			seen[e.Key] = true
			suffix, _ := suffixOf(e.Key)
			out = append(out, candidate{key: e.Key, suffix: suffix, value: e.Value})
		}
	}

	if answered == 0 {
		j.log.ErrorCtx(ctx, "journal cannot be read", logging.Err(errors.Join(errs...)))

		return nil, nil, apierr.KVUnavailable("the journal is not readable: no datacenter answers", unreachable)
	}

	if len(errs) > 0 {
		j.log.WarnCtx(ctx, "journal read without some datacenters", logging.Err(errors.Join(errs...)))
	}

	slices.SortFunc(out, func(a, b candidate) int { return strings.Compare(b.suffix, a.suffix) })

	return out, unreachable, nil
}

func requestInvalid(field, description string) error {
	return apierr.New(apierr.ReasonRequestInvalid, "invalid request", &errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: description}},
	})
}

// encodeToken returns the page token that continues below the entry name,
// for requests with filter: base64url of `<time>-<id>|<filter hash>`. The
// token is opaque to clients.
func encodeToken(name, filter string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.TrimPrefix(name, namePrefix) + "|" + filterHash(filter)))
}

func filterHash(filter string) string {
	sum := sha256.Sum256([]byte(filter))

	return hex.EncodeToString(sum[:])[:filterHashLen]
}

// decodeToken returns the `<time>-<id>` the next page continues below, or
// "" for the first page.
func decodeToken(token, filter string) (string, error) {
	if token == "" {
		return "", nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", requestInvalid("page_token", "is not a token of this service")
	}

	suffix, hash, ok := strings.Cut(string(raw), "|")
	if !ok || !keyRE.MatchString(suffix) || len(hash) != filterHashLen {
		return "", requestInvalid("page_token", "is not a token of this service")
	}

	if hash != filterHash(filter) {
		return "", requestInvalid("filter", "differs from the filter of the request that returned page_token")
	}

	return suffix, nil
}
