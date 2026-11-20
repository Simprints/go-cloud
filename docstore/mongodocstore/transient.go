// Copyright 2026 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mongodocstore

import (
	"context"
	"errors"
	"slices"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/x/mongo/driver/topology"
)

// transientCodes are the codes of the errors returned while the replica set changes its primary, or while a node
// shuts down. They are the same as in gocloud.dev/internal/mongoerr, which uses version 2 of the MongoDB driver while
// this module uses version 1.
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

// IsTransient reports whether err, or an error it wraps, is a transient error of MongoDB: a network error of the
// driver, no server selected in time (e.g. the server is down), or an error of a replica set changing its primary or of
// a node shutting down. The errors of the context of the operation aren't transient. The Go CDK has no error code for
// it (gcerrors.Code returns Unknown, as for an unavailable Google Cloud service), so callers use IsTransient to retry
// or report the error as temporary.
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
	// Unwrap() []error method, which the helpers of the driver (mongo.IsNetworkError, mongo.IsTimeout) don't follow
	// with version 1 of the driver. Only errors of the driver match: a network error of another client (e.g. a timeout
	// of an HTTP client) isn't transient here.
	var le mongo.LabeledError
	if errors.As(err, &le) && slices.ContainsFunc(transientLabels, le.HasErrorLabel) {
		return true
	}
	var se mongo.ServerError
	return errors.As(err, &se) && slices.ContainsFunc(transientCodes, se.HasErrorCode)
}
