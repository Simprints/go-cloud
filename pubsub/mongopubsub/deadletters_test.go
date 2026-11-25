package mongopubsub

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gocloud.dev/pubsub"
	"gocloud.dev/pubsub/driver"
)

// withoutDeadLetteringInterval moves the expired messages at every receive during the test.
func withoutDeadLetteringInterval(t *testing.T) {
	previous := deadLetteringInterval
	deadLetteringInterval = 0
	t.Cleanup(func() { deadLetteringInterval = previous })
}

func TestSubscriptionOptions_Retention(t *testing.T) {
	db := newTestDatabase(t)
	ctx := context.Background()

	s, err := newSubscription(ctx, db, "topic", "default", &SubscriptionOptions{AckDeadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if s.retention != DefaultRetention || DefaultRetention != 24*time.Hour {
		t.Errorf("default retention = %s, want 24h", s.retention)
	}
	s, err = newSubscription(ctx, db, "topic", "set", &SubscriptionOptions{AckDeadline: time.Minute, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if s.retention != time.Hour {
		t.Errorf("retention = %s, want 1h", s.retention)
	}
	if _, err := newSubscription(ctx, db, "topic", "negative", &SubscriptionOptions{Retention: -time.Hour}); err == nil {
		t.Error("got no error for a negative retention")
	}
}

func TestOpenSubscriptionURL_Retention(t *testing.T) {
	opener := &URLOpener{Client: newTestClient(t, serverURI)}
	db := fmt.Sprintf("pubsub-test-url-retention-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = opener.Client.Database(db).Drop(context.Background()) })

	for query, wantErr := range map[string]bool{
		"?retention=168h":                    false,
		"?retention=1h&minbackoff=1m":        false,
		"?retention=soon":                    true,
		"?retention=-1h":                     true,
		"?retention=24h&ackdeadline=10m&x=1": true,
	} {
		t.Run(query, func(t *testing.T) {
			u, err := url.Parse("mongo://" + db + "/topic/sub" + query)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := opener.OpenSubscriptionURL(context.Background(), u)
			if (err != nil) != wantErr {
				t.Fatalf("got the error %v, want an error: %v", err, wantErr)
			}
			if sub != nil {
				_ = sub.Shutdown(context.Background())
			}
		})
	}
}

// TestDeadLetters checks that a message not acked within the retention of its subscription is moved to its dead
// letters instead of being delivered again, except while a receiver has it locked, and that the dead letters can be
// listed, replayed and deleted.
func TestDeadLetters(t *testing.T) {
	withoutDeadLetteringInterval(t)
	ctx := context.Background()
	db := newTestDatabase(t)
	const retention = 2 * time.Second
	s, err := newSubscription(ctx, db, "topic", "sub", &SubscriptionOptions{AckDeadline: time.Hour, Retention: retention})
	if err != nil {
		t.Fatal(err)
	}
	topic, err := OpenTopic(db, "topic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = topic.Shutdown(ctx) }()
	send := func(body string) {
		t.Helper()
		if err := topic.Send(ctx, &pubsub.Message{Body: []byte(body), Metadata: map[string]string{"k": body}}); err != nil {
			t.Fatal(err)
		}
	}

	// "locked" is published and received alone, and kept locked: the ack deadline is an hour.
	send("locked")
	receiveBody(ctx, t, s, "locked")
	// "failing" is delivered and nacked; "other" is never received.
	send("failing")
	send("other")
	for {
		m := receiveOne(ctx, t, s)
		if err := s.SendNacks(ctx, []driver.AckID{m.AckID}); err != nil {
			t.Fatal(err)
		}
		if string(m.Body) == "failing" {
			break
		}
	}

	time.Sleep(retention + 100*time.Millisecond)
	if ms, err := s.ReceiveBatch(ctx, 10); err != nil || len(ms) != 0 {
		t.Fatalf("ReceiveBatch after the retention = %v, %v, want no message", ms, err)
	}

	letters, err := ListDeadLetters(ctx, db, "topic", "sub", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := deadLetterBodies(letters); !slices.Equal(got, []string{"failing", "other"}) {
		t.Fatalf("dead letters = %v, want failing and other (locked is locked)", got)
	}
	for _, l := range letters {
		if l.Metadata["k"] != string(l.Body) || l.DeadLetterTime.IsZero() || l.PublishTime.IsZero() {
			t.Errorf("dead letter %+v lost its metadata or times", l)
		}
		if string(l.Body) == "failing" && l.DeliveryAttempts < 1 {
			t.Errorf("dead letter failing has %d delivery attempts, want at least 1", l.DeliveryAttempts)
		}
	}
	if n, err := CountDeadLetters(ctx, db, "topic", "sub"); err != nil || n != 2 {
		t.Errorf("CountDeadLetters = %d, %v, want 2", n, err)
	}

	// Replayed, "failing" is delivered again, with its retention started again.
	failingID := letters[slices.IndexFunc(letters, func(l DeadLetter) bool { return string(l.Body) == "failing" })].ID
	if n, err := ReplayDeadLetters(ctx, db, "topic", "sub", []string{failingID}); err != nil || n != 1 {
		t.Fatalf("ReplayDeadLetters = %d, %v, want 1", n, err)
	}
	replayed := receiveBody(ctx, t, s, "failing")
	if got := replayed.AckID.(ackID).DeliveryAttempts; got != 1 {
		t.Errorf("the replayed message has %d delivery attempts, want 1", got)
	}
	if err := s.SendAcks(ctx, []driver.AckID{replayed.AckID}); err != nil {
		t.Fatal(err)
	}

	if n, err := DeleteDeadLetters(ctx, db, "topic", "sub", nil); err != nil || n != 1 {
		t.Errorf("DeleteDeadLetters = %d, %v, want 1 (other)", n, err)
	}
	if n, err := CountDeadLetters(ctx, db, "topic", "sub"); err != nil || n != 0 {
		t.Errorf("CountDeadLetters after deleting = %d, %v, want 0", n, err)
	}
}

// TestDeadLetters_ExpiredMessagesAreNotDelivered checks that a message past its
// retention isn't delivered anymore, even before it is moved to the dead
// letters: otherwise a message nacked without backoff would be locked again by
// a receive before the next move, and only moved by chance.
func TestDeadLetters_ExpiredMessagesAreNotDelivered(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	const retention = time.Second
	s, err := newSubscription(ctx, db, "topic", "sub", &SubscriptionOptions{AckDeadline: time.Hour, Retention: retention})
	if err != nil {
		t.Fatal(err)
	}
	s.lastDeadLettering = time.Now() // No move during the test: deadLetteringInterval is a minute.
	topic, err := OpenTopic(db, "topic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = topic.Shutdown(ctx) }()
	if err := topic.Send(ctx, &pubsub.Message{Body: []byte("failing")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(retention)
	for time.Now().Before(deadline) {
		ms, err := s.ReceiveBatch(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if err := s.SendNacks(ctx, []driver.AckID{m.AckID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(100 * time.Millisecond)
	if ms, err := s.ReceiveBatch(ctx, 1); err != nil || len(ms) != 0 {
		t.Fatalf("ReceiveBatch after the retention = %v, %v, want no message", ms, err)
	}
}

// TestDeadLetters_ManyMessages checks that the messages are moved, replayed and
// deleted by as many batches as needed.
func TestDeadLetters_ManyMessages(t *testing.T) {
	withoutDeadLetteringInterval(t)
	ctx := context.Background()
	db := newTestDatabase(t)
	const retention, n = time.Second, 2*deadLetteringBatchSize + 10
	s, err := newSubscription(ctx, db, "topic", "sub", &SubscriptionOptions{AckDeadline: time.Hour, Retention: retention})
	if err != nil {
		t.Fatal(err)
	}
	topic, err := OpenTopic(db, "topic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = topic.Shutdown(ctx) }()
	for i := range n {
		// The key of the metadata needs escaping in MongoDB.
		if err := topic.Send(ctx, &pubsub.Message{Body: []byte(fmt.Sprint(i)), Metadata: map[string]string{"a.b": "c"}}); err != nil {
			t.Fatal(err)
		}
	}

	time.Sleep(retention + 100*time.Millisecond)
	if ms, err := s.ReceiveBatch(ctx, 10); err != nil || len(ms) != 0 {
		t.Fatalf("ReceiveBatch after the retention = %d messages, %v, want none", len(ms), err)
	}
	letters, err := ListDeadLetters(ctx, db, "topic", "sub", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != n {
		t.Fatalf("got %d dead letters, want %d", len(letters), n)
	}
	if letters[0].Metadata["a.b"] != "c" {
		t.Errorf("dead letter metadata = %v, want a.b: c", letters[0].Metadata)
	}
	if got, err := ReplayDeadLetters(ctx, db, "topic", "sub", nil); err != nil || got != n {
		t.Fatalf("ReplayDeadLetters = %d, %v, want %d", got, err, n)
	}
	if got, err := s.subscription.CountDocuments(ctx, bson.D{}); err != nil || got != n {
		t.Fatalf("got %d messages in the subscription after the replay, %v, want %d", got, err, n)
	}

	time.Sleep(retention + 100*time.Millisecond)
	if _, err := s.ReceiveBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if got, err := DeleteDeadLetters(ctx, db, "topic", "sub", nil); err != nil || got != n {
		t.Errorf("DeleteDeadLetters = %d, %v, want %d", got, err, n)
	}
}

func TestReplayDeadLetters_UnregisteredSubscription(t *testing.T) {
	db := newTestDatabase(t)
	if _, err := ReplayDeadLetters(context.Background(), db, "topic", "nosub", nil); err == nil {
		t.Error("got no error replaying into a subscription that isn't registered")
	}
}

func TestDeleteDeadLetters_InvalidID(t *testing.T) {
	db := newTestDatabase(t)
	if _, err := DeleteDeadLetters(context.Background(), db, "topic", "sub", []string{"not-an-id"}); err == nil {
		t.Error("got no error for an invalid ID")
	}
}

func TestDeleteSubscription_DropsTheDeadLetters(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	if _, err := newSubscription(ctx, db, "topic", "sub", &SubscriptionOptions{AckDeadline: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection(deadLetterCollectionName("topic", "sub")).InsertOne(ctx, bson.D{{Key: "body", Value: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSubscription(ctx, db, "topic", "sub"); err != nil {
		t.Fatal(err)
	}
	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names, deadLetterCollectionName("topic", "sub")) {
		t.Error("the dead letters of the deleted subscription are still there")
	}
}

// receiveBody receives one message, which must have the body want.
func receiveBody(ctx context.Context, t *testing.T, s *subscription, want string) *driver.Message {
	t.Helper()
	m := receiveOne(ctx, t, s)
	if string(m.Body) != want {
		t.Fatalf("received %q, want %q", m.Body, want)
	}
	return m
}

func deadLetterBodies(letters []DeadLetter) []string {
	bodies := make([]string, len(letters))
	for i, l := range letters {
		bodies[i] = string(l.Body)
	}
	slices.Sort(bodies)
	return bodies
}

func TestListSubscriptions(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	for _, name := range []string{"b", "a"} {
		if _, err := newSubscription(ctx, db, "topic", name, &SubscriptionOptions{AckDeadline: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newSubscription(ctx, db, "another", "c", &SubscriptionOptions{AckDeadline: time.Minute}); err != nil {
		t.Fatal(err)
	}
	topic, err := OpenTopic(db, "topic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = topic.Shutdown(ctx) }()
	if err := topic.Send(ctx, &pubsub.Message{Body: []byte("m")}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection(deadLetterCollectionName("topic", "a")).InsertOne(ctx, bson.D{{Key: "body", Value: "x"}}); err != nil {
		t.Fatal(err)
	}

	subs, err := ListSubscriptions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range subs {
		got = append(got, fmt.Sprintf("%s/%s:%d:%d", s.Topic, s.Name, s.Messages, s.DeadLetters))
		if s.CreatedAt.IsZero() {
			t.Errorf("subscription %s/%s has no creation time", s.Topic, s.Name)
		}
	}
	if want := []string{"another/c:0:0", "topic/a:1:1", "topic/b:1:0"}; !slices.Equal(got, want) {
		t.Errorf("ListSubscriptions = %v, want %v", got, want)
	}
}
