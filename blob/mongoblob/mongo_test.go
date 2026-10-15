package mongoblob

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
