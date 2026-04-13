package mongopubsub

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"gocloud.dev/internal/testing/setup"
	"gocloud.dev/pubsub/driver"
	"gocloud.dev/pubsub/drivertest"
)

const (
	serverURI = "mongodb://localhost:27020/?directConnection=true"
	dbName    = "pubsub-test"
)

type harness struct {
	db        *mongo.Database
	numTopics uint32 // atomic
	numSubs   uint32 // atomic
}

func (h *harness) CreateTopic(_ context.Context, testName string) (dt driver.Topic, cleanup func(), err error) {
	topicName := fmt.Sprintf("%s-topic-%d", testName, atomic.AddUint32(&h.numTopics, 1))
	return newTopic(h.db.Collection(topicName)), func() {}, nil
}

func (h *harness) MakeNonexistentTopic(context.Context) (driver.Topic, error) {
	// A nil *topic behaves like a nonexistent topic.
	return (*topic)(nil), nil
}

func (h *harness) CreateSubscription(ctx context.Context, dt driver.Topic, testName string) (ds driver.Subscription, cleanup func(), err error) {
	t := dt.(*topic)
	subName := fmt.Sprintf("%s-subscription-%d", testName, atomic.AddUint32(&h.numSubs, 1))
	sub, err := newSubscription(ctx, t.topic, h.db.Collection(subName), time.Minute)
	if err != nil {
		return nil, nil, err
	}
	return sub, func() {}, nil
}

func (h *harness) MakeNonexistentSubscription(ctx context.Context) (ds driver.Subscription, cleanup func(), err error) {
	sub, err := newSubscription(ctx, nil, nil, 1)
	if err != nil {
		return nil, nil, err
	}
	return sub, func() {}, nil
}

func (h *harness) Close() {
	if err := h.db.Drop(context.Background()); err != nil {
		fmt.Println("error dropping database:", err)
		return
	}
	return
}

func (h *harness) MaxBatchSizes() (int, int) {
	return 0, 0
}

func (h *harness) SupportsMultipleSubscriptions() bool {
	return true
}

func TestConformance(t *testing.T) {
	client := newTestClient(t, serverURI)
	defer client.Disconnect(context.Background())

	newHarness := func(context.Context, *testing.T) (drivertest.Harness, error) {
		return &harness{db: client.Database(dbName)}, nil
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
