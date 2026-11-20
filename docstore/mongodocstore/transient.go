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

import "gocloud.dev/internal/mongoerr"

// IsTransient reports whether err, or an error it wraps, is a transient error of MongoDB: a network error of the
// driver, no server selected in time (e.g. the server is down), or an error of a replica set changing its primary or of
// a node shutting down. The errors of the context of the operation aren't transient. The Go CDK has no error code for
// it (gcerrors.Code returns Unknown, as for an unavailable Google Cloud service), so callers use IsTransient to retry
// or report the error as temporary.
func IsTransient(err error) bool {
	return mongoerr.IsTransient(err)
}
