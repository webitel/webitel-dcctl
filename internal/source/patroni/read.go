package patroni

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/webitel/webitel-dcctl/internal/source"
)

// Role is the role a member reports about itself in GET /patroni. It is not [ClusterRole]:
// the primary is "primary" here and "leader" in GET /cluster, and the two are not compared.
type Role string

const (
	RolePrimary       Role = "primary"
	RoleReplica       Role = "replica"
	RoleStandbyLeader Role = "standby_leader"
	RoleDemoted       Role = "demoted"
	RolePromoted      Role = "promoted"
	RoleUninitialized Role = "uninitialized"
)

// ClusterRole is the role of a member in GET /cluster.
type ClusterRole string

const (
	ClusterRoleLeader        ClusterRole = "leader"
	ClusterRoleStandbyLeader ClusterRole = "standby_leader"
	ClusterRoleReplica       ClusterRole = "replica"
	ClusterRoleSyncStandby   ClusterRole = "sync_standby"
	ClusterRoleQuorumStandby ClusterRole = "quorum_standby"
)

// State is the answer of GET /patroni. A nil number is unknown: with PostgreSQL down Patroni
// leaves out or nulls the timeline and the WAL positions, and zero would read as a real
// position.
type State struct {
	Role             Role
	State            string
	Timeline         *int64
	Xlog             Xlog
	ReplicationState string
	// Replication is what the member streams to others; filled on a leader.
	Replication []Replica
	// Version is the Patroni version.
	Version string
	// Pause is set while the cluster is paused.
	Pause bool
}

// WALPosition is the WAL the member holds: written by a primary, received by a replica;
// nil when it is not known.
func (s *State) WALPosition() *int64 {
	if s.Xlog.Location != nil {
		return s.Xlog.Location
	}

	return s.Xlog.ReceivedLocation
}

// Xlog holds the WAL positions of a member as byte offsets.
type Xlog struct {
	// Location is the write position of a primary.
	Location          *int64
	ReceivedLocation  *int64
	ReplayedLocation  *int64
	ReplayedTimestamp string
	Paused            bool
}

// Replica is a row of pg_stat_replication of a leader.
type Replica struct {
	Username        string
	ApplicationName string
	ClientAddr      string
	State           string
	SyncState       string
	SyncPriority    int
}

// HistoryEntry is a timeline switch from GET /history.
type HistoryEntry struct {
	Timeline  int64
	LSN       int64
	Reason    string
	Timestamp string
	NewLeader string
}

type stateWire struct {
	Role             Role   `json:"role"`
	State            string `json:"state"`
	Timeline         *int64 `json:"timeline"`
	ReplicationState string `json:"replication_state"`
	Pause            bool   `json:"pause"`
	Xlog             struct {
		Location          *int64 `json:"location"`
		ReceivedLocation  *int64 `json:"received_location"`
		ReplayedLocation  *int64 `json:"replayed_location"`
		ReplayedTimestamp string `json:"replayed_timestamp"`
		Paused            bool   `json:"paused"`
	} `json:"xlog"`
	Replication []struct {
		Username        string `json:"usename"`
		ApplicationName string `json:"application_name"`
		ClientAddr      string `json:"client_addr"`
		State           string `json:"state"`
		SyncState       string `json:"sync_state"`
		SyncPriority    int    `json:"sync_priority"`
	} `json:"replication"`
	Patroni struct {
		Version string `json:"version"`
	} `json:"patroni"`
}

// State reads GET /patroni of the member at endpoint (host:port).
func (c *Client) State(ctx context.Context, endpoint string) (State, error) {
	var w stateWire
	if err := c.call(ctx, endpoint, http.MethodGet, "/patroni", nil, &w); err != nil {
		return State{}, err
	}

	s := State{
		Role:             w.Role,
		State:            w.State,
		Timeline:         w.Timeline,
		ReplicationState: w.ReplicationState,
		Pause:            w.Pause,
		Version:          w.Patroni.Version,
		Xlog: Xlog{
			Location:          w.Xlog.Location,
			ReceivedLocation:  w.Xlog.ReceivedLocation,
			ReplayedLocation:  w.Xlog.ReplayedLocation,
			ReplayedTimestamp: w.Xlog.ReplayedTimestamp,
			Paused:            w.Xlog.Paused,
		},
	}

	for _, r := range w.Replication {
		s.Replication = append(s.Replication, Replica(r))
	}

	return s, nil
}

// Config reads GET /config, the dynamic configuration of the cluster.
func (c *Client) Config(ctx context.Context, endpoint string) (map[string]any, error) {
	var cfg map[string]any
	if err := c.call(ctx, endpoint, http.MethodGet, "/config", nil, &cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// History reads GET /history, the timeline switches of the cluster.
func (c *Client) History(ctx context.Context, endpoint string) ([]HistoryEntry, error) {
	var rows [][]json.RawMessage
	if err := c.call(ctx, endpoint, http.MethodGet, "/history", nil, &rows); err != nil {
		return nil, err
	}

	out := make([]HistoryEntry, 0, len(rows))

	for i, row := range rows {
		e, err := historyEntry(row)
		if err != nil {
			return nil, source.Classify(endpoint, fmt.Errorf("decode GET /history row %d: %w: %w", i, source.ErrInvalidResponse, err))
		}

		out = append(out, e)
	}

	return out, nil
}

// historyEntry decodes [timeline, lsn, reason, timestamp, new_leader]; the last item is optional.
func historyEntry(row []json.RawMessage) (HistoryEntry, error) {
	const required = 4

	var e HistoryEntry

	if len(row) < required {
		return e, fmt.Errorf("want at least %d items, got %d", required, len(row))
	}

	dst := []any{&e.Timeline, &e.LSN, &e.Reason, &e.Timestamp, &e.NewLeader}
	for i, raw := range row[:min(len(row), len(dst))] {
		if err := json.Unmarshal(raw, dst[i]); err != nil {
			return e, fmt.Errorf("item %d: %w", i, err)
		}
	}

	return e, nil
}

// Primary reports whether the member at endpoint is the primary: GET /primary answers 200
// on the primary and 503 on any other member, a standby leader included.
func (c *Client) Primary(ctx context.Context, endpoint string) (bool, error) {
	status, data, err := c.roundTrip(ctx, endpoint, http.MethodGet, "/primary", nil, true)
	if err != nil {
		return false, err
	}

	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusServiceUnavailable:
		return false, nil
	default:
		return false, source.Classify(endpoint, statusError(http.MethodGet, "/primary", status, data))
	}
}

// endpointOf returns host:port of an api_url from GET /cluster, or "" when it is not an https URL with a host.
// Only https is accepted so that the credentials never go in the clear.
func endpointOf(apiURL string) string {
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return ""
	}

	return u.Host
}
