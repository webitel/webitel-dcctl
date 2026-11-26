// Package cli is the command-line client of the dcctl API: the subcommands
// of webitel-dcctl that read the status and run actions over gRPC.
//
// Exit codes:
//
//	0   the operation succeeded, or a read command succeeded
//	1   the request was refused, the operation failed, or the call failed
//	2   the operation succeeded with warnings
//	3   the operation is interrupted and waits for resume or abandon
//	64  wrong usage: unknown command, unknown flag, missing argument
//	130 canceled while watching; the operation keeps running
//
// A refusal is told apart only by the reason of its google.rpc.ErrorInfo,
// never by the text of the message.
package cli
