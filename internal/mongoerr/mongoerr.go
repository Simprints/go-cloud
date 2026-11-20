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

// IsTransient reports whether err, or an error it wraps, is transient, so that the operation can be retried: a network error, no server
// selected in time (e.g. the server is down), or an error of a replica set changing its primary or of a node shutting
// down. The errors of the context of the operation aren't transient.
func IsTransient(err error) bool {
	// No server could be selected in time: the driver implements the timeout with a context, so the error also wraps
	// context.DeadlineExceeded.
	var sse topology.ServerSelectionError
	if errors.As(err, &sse) {
		return true
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if mongo.IsNetworkError(err) || mongo.IsTimeout(err) {
		return true
	}
	var se mongo.ServerError
	if !errors.As(err, &se) {
		return false
	}
	if se.HasErrorLabel("RetryableWriteError") || se.HasErrorLabel("TransientTransactionError") {
		return true
	}
	return slices.ContainsFunc(transientCodes, se.HasErrorCode)
}
