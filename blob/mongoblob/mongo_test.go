package mongoblob

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/blob/drivertest"
	"gocloud.dev/internal/testing/setup"
)

const (
	serverURIV4 = "mongodb://localhost:27017/?directConnection=true"
	dbName      = "blob-test"
	bucketName1 = "docstore-test-1"
)

type harness struct {
	db *mongo.Database
}

func (h *harness) MakeDriver(_ context.Context) (driver.Bucket, error) {
	return newBucket(h.db.GridFSBucket(options.GridFSBucket().SetName(bucketName1))), nil
}

func (h *harness) MakeDriverForNonexistentBucket(_ context.Context) (driver.Bucket, error) {
	return nil, nil
}

func (h *harness) HTTPClient() *http.Client {
	return nil
}

func (h *harness) SupportsCopy() bool {
	return true
}

func (h *harness) SupportsNonSlashDelimiters() bool {
	return true
}

func (h *harness) Close() {}

func TestConformance(t *testing.T) {
	t.Run("V4", func(t *testing.T) { testConformance(t, serverURIV4) })
}

func testConformance(t *testing.T, serverURI string) {
	client := newTestClient(t, serverURI)
	defer client.Disconnect(context.Background())

	newHarness := func(context.Context, *testing.T) (drivertest.Harness, error) {
		return &harness{client.Database(fmt.Sprintf("%s-%d", dbName, time.Now().UnixNano()))}, nil
	}
	drivertest.RunConformanceTests(t, newHarness, nil)
}

func newTestClient(t *testing.T, serverURI string) *mongo.Client {
	if !setup.HasDockerTestEnvironment() {
		t.Skip("Skipping Mongo tests since the Mongo server is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := Dial(serverURI)
	if err != nil {
		t.Fatalf("dialing to %s: %v", serverURI, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("connecting to %s: %v", serverURI, err)
	}
	return client
}

// TestList checks the listings the conformance tests don't make: without a prefix, with a prefix holding regular
// expression characters, and over several pages with a prefix.
func TestList(t *testing.T) {
	client := newTestClient(t, serverURIV4)
	defer client.Disconnect(context.Background())
	ctx := context.Background()

	db := client.Database(fmt.Sprintf("%s-list-%d", dbName, time.Now().UnixNano()))
	defer func() { _ = db.Drop(context.Background()) }()
	b, err := OpenBucket(db.GridFSBucket(options.GridFSBucket().SetName(bucketName1)))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	keys := []string{"a.b/1", "a.b/2", "a.b/3", "axb/1", "c/1"}
	for _, key := range keys {
		if err := b.WriteAll(ctx, key, []byte(key), nil); err != nil {
			t.Fatalf("writing %s: %v", key, err)
		}
	}

	tests := map[string]struct {
		prefix   string
		pageSize int
		want     []string
	}{
		"no prefix":                      {want: keys},
		"no prefix, several pages":       {pageSize: 2, want: keys},
		"prefix with a regexp character": {prefix: "a.b/", want: []string{"a.b/1", "a.b/2", "a.b/3"}},
		"prefix over several pages":      {prefix: "a.b/", pageSize: 1, want: []string{"a.b/1", "a.b/2", "a.b/3"}},
		"prefix matching no key":         {prefix: "z", want: nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var got []string
			opts := &blob.ListOptions{Prefix: tc.prefix}
			if tc.pageSize == 0 {
				iter := b.List(opts)
				for {
					obj, err := iter.Next(ctx)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatalf("listing: %v", err)
					}
					got = append(got, obj.Key)
				}
			} else {
				token := blob.FirstPageToken
				for {
					objs, next, err := b.ListPage(ctx, token, tc.pageSize, opts)
					if err != nil {
						t.Fatalf("listing a page: %v", err)
					}
					for _, obj := range objs {
						got = append(got, obj.Key)
					}
					if len(next) == 0 {
						break
					}
					token = next
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
