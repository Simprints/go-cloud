package mongopubsub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/testing/setup"
	"gocloud.dev/pubsub"
	"gocloud.dev/pubsub/driver"
	"gocloud.dev/pubsub/drivertest"
)

// The tests need a replica set, for the transactions, such as the one of
// docstore/mongodocstore/docker-compose.yml.
const (
	serverURI = "mongodb://localhost:27017/?directConnection=true"
	dbName    = "pubsub-test"
)

type harness struct {
	db        *mongo.Database
	numTopics uint32 // atomic
	numSubs   uint32 // atomic
}

func (h *harness) CreateTopic(_ context.Context, testName string) (dt driver.Topic, cleanup func(), err error) {
	topicName := fmt.Sprintf("%s-topic-%d", testName, atomic.AddUint32(&h.numTopics, 1))
	return newTopic(h.db, topicName), func() {}, nil
}

func (h *harness) MakeNonexistentTopic(context.Context) (driver.Topic, error) {
	// A nil *topic behaves like a nonexistent topic.
	return (*topic)(nil), nil
}

func (h *harness) CreateSubscription(ctx context.Context, dt driver.Topic, testName string) (ds driver.Subscription, cleanup func(), err error) {
	t := dt.(*topic)
	subName := fmt.Sprintf("%s-subscription-%d", testName, atomic.AddUint32(&h.numSubs, 1))
	sub, err := newSubscription(ctx, h.db, t.name, subName, time.Minute)
	if err != nil {
		return nil, nil, err
	}
	return sub, func() {}, nil
}

func (h *harness) MakeNonexistentSubscription(ctx context.Context) (ds driver.Subscription, cleanup func(), err error) {
	sub, err := newSubscription(ctx, nil, "", "", 1)
	if err != nil {
		return nil, nil, err
	}
	return sub, func() {}, nil
}

func (h *harness) Close() {
	if err := h.db.Drop(context.Background()); err != nil {
		fmt.Println("error dropping database:", err)
	}
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

// TestPublishWhileNoSubscriberIsRunning checks that the messages published while
// a registered subscription isn't open are delivered once it is opened again.
func TestPublishWhileNoSubscriberIsRunning(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	sub, err := OpenSubscription(ctx, db, "topic", "sub", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	topic, err := OpenTopic(db, "topic")
	if err != nil {
		t.Fatal(err)
	}
	defer topic.Shutdown(ctx)
	const numMessages = 10
	for i := range numMessages {
		if err := topic.Send(ctx, &pubsub.Message{Body: []byte(fmt.Sprint(i))}); err != nil {
			t.Fatal(err)
		}
	}

	sub, err = OpenSubscription(ctx, db, "topic", "sub", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Shutdown(ctx)
	received := map[string]bool{}
	for len(received) < numMessages {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		m, err := sub.Receive(ctx)
		cancel()
		if err != nil {
			t.Fatalf("received %d messages out of %d: %v", len(received), numMessages, err)
		}
		received[string(m.Body)] = true
		m.Ack()
	}
}

// TestConcurrentReceivers checks that receivers of the same subscription, as
// several instances of a service, don't receive the same messages before their
// ack deadline, and that every message is received.
func TestConcurrentReceivers(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	const numReceivers = 4
	subs := make([]*pubsub.Subscription, numReceivers)
	for i := range subs {
		var err error
		if subs[i], err = OpenSubscription(ctx, db, "topic", "sub", time.Minute); err != nil {
			t.Fatal(err)
		}
		defer subs[i].Shutdown(ctx)
	}
	topic, err := OpenTopic(db, "topic")
	if err != nil {
		t.Fatal(err)
	}
	defer topic.Shutdown(ctx)
	const numMessages = 200
	for i := range numMessages {
		if err := topic.Send(ctx, &pubsub.Message{Body: []byte(fmt.Sprint(i))}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	received := map[string]int{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Go(func() {
			for {
				m, err := sub.Receive(ctx)
				if err != nil {
					return
				}
				m.Ack()
				mu.Lock()
				received[string(m.Body)]++
				if len(received) == numMessages {
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	go func() {
		time.Sleep(20 * time.Second)
		cancel()
	}()
	wg.Wait()

	if len(received) != numMessages {
		t.Errorf("received %d messages, want %d", len(received), numMessages)
	}
	for body, n := range received {
		if n != 1 {
			t.Errorf("message %s received %d times, want once", body, n)
		}
	}
}

// TestStaleNack checks that nacking a message whose ack deadline expired, and
// that has been received again, doesn't unlock it.
func TestStaleNack(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	sub, err := newSubscription(ctx, db, "topic", "sub", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := newTopic(db, "topic").SendBatch(ctx, []*driver.Message{{Body: []byte("hello")}}); err != nil {
		t.Fatal(err)
	}
	first := receiveOne(ctx, t, sub)
	time.Sleep(1100 * time.Millisecond) // The ack deadline expires.
	second := receiveOne(ctx, t, sub)

	if err := sub.SendNacks(ctx, []driver.AckID{first.AckID}); err != nil {
		t.Fatal(err)
	}
	if ms, err := sub.ReceiveBatch(ctx, 1); err != nil || len(ms) != 0 {
		t.Fatalf("ReceiveBatch after a stale nack = %v, %v, want no message", ms, err)
	}
	if err := sub.SendNacks(ctx, []driver.AckID{second.AckID}); err != nil {
		t.Fatal(err)
	}
	receiveOne(ctx, t, sub)
}

// TestConcurrentRegistration checks that a subscription can be opened
// concurrently, as several instances of a service starting at the same time.
func TestConcurrentRegistration(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			_, err := registerSubscription(ctx, db, "topic", "sub")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	n, err := db.Collection(subscriptionsCollection).CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d subscriptions registered, want 1", n)
	}
}

// TestFanOut checks that the messages are delivered to every subscription of
// their topic, and only to them.
func TestFanOut(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	sub1, err := newSubscription(ctx, db, "topic", "sub1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sub2, err := newSubscription(ctx, db, "topic", "sub2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newSubscription(ctx, db, "other-topic", "sub", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := newTopic(db, "topic").SendBatch(ctx, []*driver.Message{{Body: []byte("hello")}}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*subscription{sub1, sub2} {
		if m := receiveOne(ctx, t, sub); string(m.Body) != "hello" {
			t.Errorf("received %q, want hello", m.Body)
		}
	}
	if ms, err := other.ReceiveBatch(ctx, 1); err != nil || len(ms) != 0 {
		t.Errorf("ReceiveBatch on the subscription of another topic = %v, %v, want no message", ms, err)
	}

	if err := DeleteSubscription(ctx, db, "topic", "sub2"); err != nil {
		t.Fatal(err)
	}
	if err := newTopic(db, "topic").SendBatch(ctx, []*driver.Message{{Body: []byte("again")}}); err != nil {
		t.Fatal(err)
	}
	if n, err := db.Collection(subscriptionCollectionName("topic", "sub2")).CountDocuments(ctx, bson.D{}); err != nil || n != 0 {
		t.Errorf("%d messages in the deleted subscription (err %v), want 0", n, err)
	}
}

// TestSendWithoutSubscription checks that publishing to a topic without
// subscriptions succeeds.
func TestSendWithoutSubscription(t *testing.T) {
	db := newTestDatabase(t)
	if err := newTopic(db, "topic").SendBatch(context.Background(), []*driver.Message{{Body: []byte("hello")}}); err != nil {
		t.Fatal(err)
	}
}

func TestIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not a mongo error", errors.New("boom"), false},
		{"canceled", context.Canceled, false},
		{"not writable primary", mongo.CommandError{Code: 10107}, true},
		{"primary stepped down", mongo.CommandError{Code: 189}, true},
		{"interrupted by a replica set state change", mongo.CommandError{Code: 11602}, true},
		{"transient transaction error", mongo.CommandError{Code: 112, Labels: []string{"TransientTransactionError"}}, true},
		{"network error", mongo.CommandError{Labels: []string{"NetworkError"}}, true},
		{"duplicate key", mongo.CommandError{Code: 11000}, false},
		{"wrapped", fmt.Errorf("inserting: %w", mongo.CommandError{Code: 10107}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	if got := errorCode(fmt.Errorf("receiving: %w", context.Canceled)); got != gcerrors.Canceled {
		t.Errorf("errorCode(context.Canceled) = %v, want Canceled", got)
	}
}

func receiveOne(ctx context.Context, t *testing.T, sub *subscription) *driver.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ms, err := sub.ReceiveBatch(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) == 1 {
			return ms[0]
		}
	}
	t.Fatal("no message received")
	return nil
}

func newTestDatabase(t *testing.T) *mongo.Database {
	t.Helper()
	client := newTestClient(t, serverURI)
	db := client.Database(fmt.Sprintf("pubsub-test-%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		ctx := context.Background()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	return db
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
