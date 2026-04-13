// Copyright 2019 The Go Cloud Development Kit Authors
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

package mongoblob

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"gocloud.dev/blob"
)

func init() {
	blob.DefaultURLMux().RegisterBucket(Scheme, new(defaultDialer))
}

// defaultDialer dials a default Mongo server based on the environment variable
// MONGO_SERVER_URL.
type defaultDialer struct {
	mongoServerURL string
	mu             sync.Mutex
	opener         *URLOpener
	err            error
}

func (o *defaultDialer) OpenBucketURL(ctx context.Context, u *url.URL) (*blob.Bucket, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	currentEnv := os.Getenv("MONGO_SERVER_URL")

	if currentEnv == "" {
		o.err = errors.New("MONGO_SERVER_URL environment variable is not set")
		return nil, fmt.Errorf("open collection %s: %v", u, o.err)
	}

	// If MONGO_SERVER_URL has been updated, then update o.opener as well
	if currentEnv != o.mongoServerURL {
		client, err := Dial(currentEnv)
		if err != nil {
			o.err = fmt.Errorf("failed to dial default Mongo server at %q: %v", currentEnv, err)
			return nil, fmt.Errorf("open collection %s: %v", u, o.err)
		}
		o.mongoServerURL = currentEnv
		o.opener = &URLOpener{Client: client}
	}
	return o.opener.OpenBucketURL(ctx, u)
}

// Scheme is the URL scheme mongoblob registers its URLOpener under on
// docstore.DefaultMux.
const Scheme = "mongo"

// URLOpener opens URLs like "mongo://mydb/mybucket".
// See https://docs.mongodb.com/manual/reference/limits/#naming-restrictions for
// naming restrictions.
//
// The URL Host is used as the database name.
// The URL Path is used as the bucket name.
type URLOpener struct {
	// A Client is a MongoDB client that performs operations on the db, must be
	// non-nil.
	Client *mongo.Client
}

// OpenBucketURL opens the Bucket URL.
func (o *URLOpener) OpenBucketURL(_ context.Context, u *url.URL) (*blob.Bucket, error) {
	dbName := u.Host
	if dbName == "" {
		return nil, fmt.Errorf("open bucket %s: URL must have a non-empty Host (database name)", u)
	}
	bucketName := strings.TrimPrefix(u.Path, "/")
	if bucketName == "" {
		return nil, fmt.Errorf("open bucket %s: URL must have a non-empty Path (bucket name)", u)
	}
	return OpenBucket(o.Client.Database(dbName).GridFSBucket(options.GridFSBucket().SetName(bucketName)))
}
