// Package answer turns the verdict of a preflight into what the API answers an action
// with: the planned operation of a validate_only request, or the refusal that the
// request and its validate_only get alike.
//
// A refusal is a gRPC status with google.rpc.ErrorInfo, google.rpc.PreconditionFailure
// and webitel.dcctl.v1.Preflight. Its reason is PREFLIGHT_FAILED, or
// CONFIRMATION_REQUIRED when only confirmations are missing, unless one of the
// coordination checks failed. Those give their own reason; when several fail, the first
// of this list wins:
//
//  1. coord.no_operation found an operation: OPERATION_RUNNING. A copy of dcctl/op
//     that cannot be read is not one, there is no operation to go to;
//  2. coord.lock_free found a holder: LOCK_BUSY;
//  3. coord.lock_free or coord.no_operation got no answer from any Consul KV:
//     LOCK_UNAVAILABLE;
//  4. coord.leader_reachable: LEADER_UNAVAILABLE.
//
// What is known about the state comes first: a running operation usually holds the lock
// too, and the client goes to the operation rather than retrying. Sources that do not
// answer come after.
package answer
