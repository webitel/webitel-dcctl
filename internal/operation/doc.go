// Package operation runs every dcctl action as an operation: a plan of steps
// that the leading instance runs in order under dcctl/lock, recording every
// change in dcctl/op, the journal and an in-process bus.
//
// A scenario builds a [Plan] and passes it to [Engine.Start]. The engine runs
// the preflight, takes the lock, checks that no other operation is running,
// writes the operation and runs the steps in the background. Before each step
// it checks that the lock is still held and decides the confirmations of the
// step again on the checks [Step.Recheck] returns.
//
// A step that fails before the point of no return is rolled back with the
// steps before it, in reverse, and the operation ends FAILED. A failure of the
// step marked as the point of no return or of a later one, a failed rollback,
// an outcome that is not known, a lost lock or a stopping instance leave the
// operation INTERRUPTED: the infrastructure is in between, and the operator
// decides with [Engine.Resume] or [Engine.Abandon].
//
// [Engine.Run] keeps the operations of all instances moving to a decision: an
// operation of this instance found RUNNING after a restart is interrupted at
// once, and a RUNNING operation of another instance is interrupted when this
// instance can take dcctl/lock for it: no live session holds the lock in the
// KVs that answered. It also aligns the copies of dcctl/op and the voters.
package operation
