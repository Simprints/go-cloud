// Package mongoerr classifies the errors of the MongoDB driver (v2) for the MongoDB drivers of the Go CDK.
//
// The Go CDK has no error code for an unavailable service (the Google Cloud drivers report it as Unknown too), so the
// drivers export IsTransient for their callers to recognise the transient errors, as they would check the status of a
// Google Cloud error with ErrorAs.
package mongoerr

import (
	"context"
	"errors"
	"slices"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"
)

// transientCodes are the codes of the errors returned while the replica set changes its primary, or while a node
// shuts down.
var transientCodes = []int{
	6,     // HostUnreachable
	7,     // HostNotFound
	89,    // NetworkTimeout
	91,    // ShutdownInProgress
	189,   // PrimarySteppedDown
	262,   // ExceededTimeLimit
	9001,  // SocketException
	10107, // NotWritablePrimary
	11600, // InterruptedAtShutdown
	11602, // InterruptedDueToReplStateChange
	13435, // NotPrimaryNoSecondaryOk
	13436, // NotPrimaryOrSecondary
}

// transientLabels are the labels of the network errors and of the errors the driver itself considers retryable.
var transientLabels = []string{"NetworkError", "RetryableWriteError", "TransientTransactionError"}

// IsTransient reports whether err, or an error it wraps, is transient, so that the operation can be retried: a network
// error of the driver, no server selected in time (e.g. the server is down), or an error of a replica set changing its
// primary or of a node shutting down. The errors of the context of the operation aren't transient, except a deadline
// reached while waiting for a server, which the driver reports as a server selection error that can't be told apart
// from the server selection timeout.
func IsTransient(err error) bool {
	// The operation was cancelled by its caller, possibly while waiting for a server.
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	// No server could be selected in time: the driver implements the timeout with a context, so the error also wraps
	// context.DeadlineExceeded.
	var sse topology.ServerSelectionError
	if errors.As(err, &sse) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// The labels and codes are found with errors.As, as the Go CDK wraps the errors of the driver in errors with an
	// Unwrap() []error method. Only errors of the driver match: a network error of another client (e.g. a timeout of an
	// HTTP client) isn't transient here.
	var le mongo.LabeledError
	if errors.As(err, &le) && slices.ContainsFunc(transientLabels, le.HasErrorLabel) {
		return true
	}
	var se mongo.ServerError
	return errors.As(err, &se) && slices.ContainsFunc(transientCodes, se.HasErrorCode)
}
