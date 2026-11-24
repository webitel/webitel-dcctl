package patroni

import (
	"context"
	"errors"
	"net/http"

	"github.com/webitel/webitel-dcctl/internal/source"
)

// PatchConfig sends PATCH /config, a JSON merge patch of the dynamic configuration: a nil
// value removes a key. It returns the configuration after the change.
func (c *Client) PatchConfig(ctx context.Context, endpoint string, patch map[string]any) (map[string]any, error) {
	var cfg map[string]any
	if err := c.call(ctx, endpoint, http.MethodPatch, "/config", patch, &cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Switchover sends POST /switchover to hand the leadership over from leader; an empty
// candidate lets Patroni choose. It returns the text Patroni answers with.
//
// Patroni holds the request until the switchover is done, so only the context of the
// caller bounds it. Any error, 503 "status unknown" included, means the result is unknown:
// re-read the cluster with [Client.Cluster]. A 200 does not mean that candidate became the
// leader either, Patroni also answers 200 to "Switched over to X instead of Y".
func (c *Client) Switchover(ctx context.Context, endpoint, leader, candidate string) (string, error) {
	if leader == "" {
		return "", errors.New("patroni: switchover needs the current leader")
	}

	req := struct {
		Leader    string `json:"leader"`
		Candidate string `json:"candidate,omitempty"`
	}{leader, candidate}

	return c.hold(ctx, endpoint, "/switchover", req)
}

// Failover sends POST /failover to promote candidate when the cluster has no leader. It
// returns the text Patroni answers with.
//
// Like [Client.Switchover] it is bounded by the context of the caller only, any error means
// the result is unknown and a 200 does not guarantee that candidate became the leader:
// re-read the cluster with [Client.Cluster].
func (c *Client) Failover(ctx context.Context, endpoint, candidate string) (string, error) {
	if candidate == "" {
		return "", errors.New("patroni: failover needs a candidate")
	}

	req := struct {
		Candidate string `json:"candidate"`
	}{candidate}

	return c.hold(ctx, endpoint, "/failover", req)
}

// hold posts in without the request timeout and returns the body of a 2xx answer.
func (c *Client) hold(ctx context.Context, endpoint, path string, in any) (string, error) {
	status, data, err := c.roundTrip(ctx, endpoint, http.MethodPost, path, in, false)
	if err != nil {
		return "", err
	}

	if status < 200 || status > 299 {
		return "", source.Classify(endpoint, statusError(http.MethodPost, path, status, data))
	}

	return printable(data[:min(len(data), maxErrBody)]), nil
}
