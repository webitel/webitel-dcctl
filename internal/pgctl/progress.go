package pgctl

import "github.com/webitel/webitel-dcctl/internal/source/patroni"

// Action is what a primitive does.
type Action string

const (
	ActionDemote     Action = "demote"
	ActionCatchup    Action = "catchup"
	ActionPromote    Action = "promote"
	ActionRepoint    Action = "repoint"
	ActionFollow     Action = "follow"
	ActionSwitchover Action = "switchover"
	ActionFailover   Action = "failover"
	ActionNoPrimary  Action = "no_primary"
)

// Phase is where a primitive is.
type Phase string

const (
	// PhaseCheck checks that the cluster is the one to change and may be changed.
	PhaseCheck Phase = "check"
	// PhaseRequest sends the request.
	PhaseRequest Phase = "request"
	// PhaseWait polls the condition.
	PhaseWait   Phase = "wait"
	PhaseDone   Phase = "done"
	PhaseFailed Phase = "failed"
)

// RequestState is what is known about the request of a primitive.
type RequestState string

const (
	// RequestNone is a primitive that sends nothing, one that found its condition met
	// already, or one that ended before sending.
	RequestNone RequestState = "none"
	// RequestApplied is a request Patroni answered with 2xx.
	RequestApplied RequestState = "applied"
	// RequestUnknown is a request in flight, or one that failed after it may have been
	// sent: it may have been applied.
	RequestUnknown RequestState = "unknown"
	// RequestRefused is a request Patroni answered with 4xx when no earlier attempt had
	// an unknown outcome.
	RequestRefused RequestState = "refused"
)

// Observation is what the last poll saw of the member the condition is about.
type Observation struct {
	// Leader is the leader of the cluster by GET /cluster; empty when there is none or
	// it could not be read.
	Leader string
	// Member is the member the condition is about: the leader, the standby leader of the
	// target or the candidate.
	Member string
	// Role and Timeline are from GET /patroni of Member; empty and nil when unknown.
	Role     patroni.Role
	Timeline *int64
	// Lag is how many bytes of WAL Member still lacks; nil when unknown or not applicable.
	Lag *int64
	// Detail says what the condition still lacks; empty when it is met.
	Detail string
	// Err is the last error reading the state.
	Err error
}

// Progress is reported when a primitive changes phase and after every poll.
type Progress struct {
	Observation

	Datacenter string
	Action     Action
	Phase      Phase
}

// Result is how a primitive ended.
type Result struct {
	// Observation is the last one; on success its Detail is empty.
	Observation

	Datacenter string
	Action     Action
	Request    RequestState
	// FromPrimary is set by [Controller.Promote] when the leader was primary already: the
	// request only removed standby_cluster, nothing was promoted.
	FromPrimary bool
	// Unread are the clusters [Controller.NoPrimary] did not read among those it does not
	// need.
	Unread []string
}
