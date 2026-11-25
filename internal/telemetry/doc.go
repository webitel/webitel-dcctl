// Package telemetry exports what dcctl observes for monitoring: the metrics of the
// installation read from the status of the last collection cycle, the durations of
// operations and their steps, and the health checks of serve. The metrics and checks
// read only what the collection loop observed and never ask a source themselves.
package telemetry
