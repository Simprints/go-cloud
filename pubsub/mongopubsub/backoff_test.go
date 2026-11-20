package mongopubsub

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gocloud.dev/pubsub/driver"
)

func TestBackoff(t *testing.T) {
	s := &subscription{minBackoff: time.Second, maxBackoff: 10 * time.Second}
	for attempts, want := range map[int]time.Duration{
		0:    time.Second, // a message stored before the attempts were counted
		1:    time.Second,
		2:    2 * time.Second,
		3:    4 * time.Second,
		4:    8 * time.Second,
		5:    10 * time.Second,
		1000: 10 * time.Second,
	} {
		if got := s.backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %s, want %s", attempts, got, want)
		}
	}
}

func TestSubscriptionOptions(t *testing.T) {
	tests := map[string]struct {
		opts           SubscriptionOptions
		wantErr        bool
		wantMaxBackoff time.Duration
	}{
		"no backoff":                    {opts: SubscriptionOptions{}},
		"minimum backoff only":          {opts: SubscriptionOptions{MinBackoff: time.Minute}, wantMaxBackoff: 10 * time.Minute},
		"minimum and maximum backoff":   {opts: SubscriptionOptions{MinBackoff: time.Minute, MaxBackoff: 5 * time.Minute}, wantMaxBackoff: 5 * time.Minute},
		"maximum without minimum":       {opts: SubscriptionOptions{MaxBackoff: time.Minute}, wantErr: true},
		"maximum less than the minimum": {opts: SubscriptionOptions{MinBackoff: time.Minute, MaxBackoff: time.Second}, wantErr: true},
		"negative minimum":              {opts: SubscriptionOptions{MinBackoff: -time.Second}, wantErr: true},
		"maximum equal to the minimum":  {opts: SubscriptionOptions{MinBackoff: time.Minute, MaxBackoff: time.Minute}, wantMaxBackoff: time.Minute},
	}
	db := newTestDatabase(t)
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s, err := newSubscription(context.Background(), db, "topic", "sub", &tc.opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got the error %v, want an error: %v", err, tc.wantErr)
			}
			if err == nil && s.maxBackoff != tc.wantMaxBackoff {
				t.Errorf("got the maximum backoff %s, want %s", s.maxBackoff, tc.wantMaxBackoff)
			}
		})
	}
}

func TestOpenSubscriptionURL_Backoff(t *testing.T) {
	opener := &URLOpener{Client: newTestClient(t, serverURI)}
	db := fmt.Sprintf("pubsub-test-url-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = opener.Client.Database(db).Drop(context.Background()) })

	tests := map[string]struct {
		query   string
		wantErr bool
	}{
		"no backoff":                    {query: ""},
		"minimum and maximum backoff":   {query: "?minbackoff=1m&maxbackoff=10m"},
		"with an ack deadline":          {query: "?ackdeadline=10m&minbackoff=1m"},
		"invalid minimum backoff":       {query: "?minbackoff=soon", wantErr: true},
		"maximum without minimum":       {query: "?maxbackoff=10m", wantErr: true},
		"maximum less than the minimum": {query: "?minbackoff=10m&maxbackoff=1m", wantErr: true},
		"unknown parameter":             {query: "?backoff=1m", wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse("mongo://" + db + "/topic/sub" + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := opener.OpenSubscriptionURL(context.Background(), u)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got the error %v, want an error: %v", err, tc.wantErr)
			}
			if sub != nil {
				_ = sub.Shutdown(context.Background())
			}
		})
	}
}

// TestNackBackoff checks that a nacked message stays locked for the backoff, doubling with each delivery up to the
// maximum, and is delivered again afterwards.
func TestNackBackoff(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	sub, err := newSubscription(ctx, db, "topic", "sub",
		&SubscriptionOptions{AckDeadline: time.Minute, MinBackoff: 300 * time.Millisecond, MaxBackoff: 600 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := newTopic(db, "topic").SendBatch(ctx, []*driver.Message{{Body: []byte("hello")}}); err != nil {
		t.Fatal(err)
	}

	// The backoff after each delivery: the minimum, doubled, then capped at the maximum.
	for delivery, wantBackoff := range []time.Duration{300 * time.Millisecond, 600 * time.Millisecond, 600 * time.Millisecond} {
		m := receiveOne(ctx, t, sub)
		if got := m.AckID.(ackID).DeliveryAttempts; got != delivery+1 {
			t.Errorf("got %d delivery attempts, want %d", got, delivery+1)
		}
		nackedAt := time.Now()
		if err := sub.SendNacks(ctx, []driver.AckID{m.AckID}); err != nil {
			t.Fatal(err)
		}

		var stored message
		if err := sub.subscription.FindOne(ctx, bson.D{{Key: "_id", Value: m.AckID.(ackID).ID}}).Decode(&stored); err != nil {
			t.Fatal(err)
		}
		// MongoDB stores dates to the millisecond.
		if lockedFor := stored.LockedUntil.Sub(nackedAt); lockedFor < wantBackoff-10*time.Millisecond || lockedFor > wantBackoff+time.Second {
			t.Errorf("delivery %d: the message is locked for %s after the nack, want about %s", delivery+1, lockedFor, wantBackoff)
		}
		if ms, err := sub.ReceiveBatch(ctx, 1); err != nil || len(ms) != 0 {
			t.Fatalf("delivery %d: ReceiveBatch right after the nack = %v, %v, want no message", delivery+1, ms, err)
		}
	}
	// It is delivered again once the backoff expired.
	receiveOne(ctx, t, sub)
}
