// Package mongopubsub provides a pubsub implementation backed by MongoDB.
//
// It requires a MongoDB replica set, since publishing relies on multi-document
// transactions.
//
// # Design
//
// Messages are fanned out at publish time:
//
//   - Every subscription is recorded in the "_subscriptions" collection of the
//     topic's database, the first time it is opened, and gets its own
//     collection named "<topic>_<subscription>".
//   - Topic.Send inserts the messages into the collection of every subscription
//     registered for the topic, in a single transaction committed with a majority
//     write concern. Once Send returns successfully, the messages are durable in
//     every subscription, whether or not a subscriber is running.
//   - A subscription only receives the messages published after it was first
//     opened, like Google Cloud Pub/Sub. Messages published to a topic without
//     any subscription are discarded.
//   - Receivers lock the messages they receive until the ack deadline. Acked
//     messages are deleted, nacked messages and messages whose ack deadline
//     expired are delivered again: delivery is at-least-once.
//   - A nacked message is delivered again at once, unless the subscription has
//     a backoff (SubscriptionOptions.MinBackoff, or the minbackoff query
//     parameter): it is then delivered again after a delay doubling with each
//     delivery of the message, from the minimum to the maximum backoff, like
//     the retry policy of Google Cloud Pub/Sub.
//   - A message is retained for delivery for a period after it was published
//     (SubscriptionOptions.Retention, or the retention query parameter, 24
//     hours by default). A message still not acked by then, e.g. one that a bug
//     keeps failing, or one that nothing received in time, is moved to the dead
//     letters of the subscription, a
//     collection named "<topic>_<subscription>_deadletters", rather than
//     delivered again forever: see ListDeadLetters, ReplayDeadLetters and
//     DeleteDeadLetters.
//
// Several processes, possibly from different services, can publish to the same
// topics and receive from the same subscriptions concurrently.
//
// A subscription keeps accumulating messages as long as it is registered, even
// if nothing receives from it anymore: delete the subscriptions that are no
// longer used with DeleteSubscription.
//
// # URLs
//
// For pubsub.OpenTopic and pubsub.OpenSubscription, mongopubsub registers for
// the scheme "mongo". The default URL opener connects to the server whose URI is
// in the environment variable MONGO_SERVER_URL. Topics are "mongo://mydb/mytopic"
// and subscriptions are "mongo://mydb/mytopic/mysub". See URLOpener for the
// supported query parameters.
package mongopubsub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/escape"
	"gocloud.dev/internal/gcerr"
	"gocloud.dev/internal/mongoerr"
	"gocloud.dev/pubsub"
	"gocloud.dev/pubsub/driver"
)

func init() {
	o := new(defaultDialer)
	pubsub.DefaultURLMux().RegisterTopic(Scheme, o)
	pubsub.DefaultURLMux().RegisterSubscription(Scheme, o)
}

// defaultDialer dials a default Mongo server based on the environment variable
// MONGO_SERVER_URL.
type defaultDialer struct {
	mongoServerURL string
	mu             sync.Mutex
	opener         *URLOpener
}

func (o *defaultDialer) OpenTopicURL(ctx context.Context, u *url.URL) (*pubsub.Topic, error) {
	opener, err := o.urlOpener(ctx)
	if err != nil {
		return nil, fmt.Errorf("open topic %s: %v", u, err)
	}
	return opener.OpenTopicURL(ctx, u)
}

func (o *defaultDialer) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*pubsub.Subscription, error) {
	opener, err := o.urlOpener(ctx)
	if err != nil {
		return nil, fmt.Errorf("open subscription %s: %v", u, err)
	}
	return opener.OpenSubscriptionURL(ctx, u)
}

// urlOpener returns the URLOpener for the server in MONGO_SERVER_URL, dialing it
// again if MONGO_SERVER_URL changed.
func (o *defaultDialer) urlOpener(ctx context.Context) (*URLOpener, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	currentEnv := os.Getenv("MONGO_SERVER_URL")
	if currentEnv == "" {
		return nil, errors.New("MONGO_SERVER_URL environment variable is not set")
	}
	if currentEnv != o.mongoServerURL {
		client, err := Dial(currentEnv)
		if err != nil {
			return nil, fmt.Errorf("failed to dial default Mongo server at %q: %v", currentEnv, err)
		}
		if o.opener != nil {
			_ = o.opener.Client.Disconnect(ctx)
		}
		o.mongoServerURL = currentEnv
		o.opener = &URLOpener{Client: client}
	}
	return o.opener, nil
}

// Scheme is the URL scheme mongopubsub registers its URLOpener under on
// pubsub.DefaultURLMux.
const Scheme = "mongo"

// URLOpener opens URLs like "mongo://mydb/mytopic" for topics or "mongo://mydb/mytopic/mysub" for subscriptions.
// See https://docs.mongodb.com/manual/reference/limits/#naming-restrictions for
// naming restrictions.
//
// Query parameters of the subscriptions, in time.ParseDuration formats:
//   - ackdeadline: The ack deadline. Defaults to 5m.
//   - minbackoff: The delay before a nacked message is delivered again the
//     first time, doubling with each delivery of the message. Defaults to 0: a
//     nacked message is delivered again at once.
//   - maxbackoff: The maximum delay before a nacked message is delivered again.
//     Defaults to 10 times minbackoff. Requires minbackoff.
//   - retention: How long a message is retained for delivery after it was
//     published, before it is moved to the dead letters of the subscription.
//     Defaults to 24h.
type URLOpener struct {
	// A Client is a MongoDB client that performs operations on the db, must be
	// non-nil.
	Client *mongo.Client
}

func (o *URLOpener) OpenTopicURL(_ context.Context, u *url.URL) (*pubsub.Topic, error) {
	dbName := u.Host
	if dbName == "" {
		return nil, fmt.Errorf("open topic %s: URL must have a non-empty Host (database name)", u)
	}
	topicName := strings.TrimPrefix(u.Path, "/")
	if topicName == "" {
		return nil, fmt.Errorf("open topic %s: URL must have a non-empty Path (topic name)", u)
	}
	return OpenTopic(o.Client.Database(dbName), topicName)
}

func (o *URLOpener) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*pubsub.Subscription, error) {
	q := u.Query()

	opts := SubscriptionOptions{AckDeadline: 5 * time.Minute}
	for param, dest := range map[string]*time.Duration{
		"ackdeadline": &opts.AckDeadline,
		"minbackoff":  &opts.MinBackoff,
		"maxbackoff":  &opts.MaxBackoff,
		"retention":   &opts.Retention,
	} {
		if s := q.Get(param); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				return nil, fmt.Errorf("open subscription %v: invalid %s %q: %v", u, param, s, err)
			}
			*dest = d
		}
		q.Del(param)
	}
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("open subscription %v: %w", u, err)
	}
	for param := range q {
		return nil, fmt.Errorf("open subscription %v: invalid query parameter %q", u, param)
	}
	dbName := u.Host
	if dbName == "" {
		return nil, fmt.Errorf("open subscription %s: URL must have a non-empty Host (database name)", u)
	}
	pathSplit := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(pathSplit) != 2 || pathSplit[0] == "" || pathSplit[1] == "" {
		return nil, fmt.Errorf("open subscription %s: URL path must be in the format /topic/subscription", u)
	}
	return OpenSubscriptionWithOptions(ctx, o.Client.Database(dbName), pathSplit[0], pathSplit[1], &opts)
}

// Dial returns a new mongoDB client that is connected to the server URI.
func Dial(uri string) (*mongo.Client, error) {
	opts := options.Client().ApplyURI(uri)
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	return client, nil
}

var errNotExist = errors.New("mongopubsub: topic does not exist")

// subscriptionsCollection is the name of the collection, in the topics'
// database, where the subscriptions are registered.
const subscriptionsCollection = "_subscriptions"

// messageVersion is the version of the message document format.
const messageVersion = 1

// subscriptionRegistration is the document registering a subscription in the
// subscriptionsCollection.
type subscriptionRegistration struct {
	ID         string    `bson:"_id"`
	Topic      string    `bson:"topic"`
	Collection string    `bson:"collection"`
	CreatedAt  time.Time `bson:"createdAt"`
}

func subscriptionID(topicName, subName string) string {
	return topicName + "/" + subName
}

func subscriptionCollectionName(topicName, subName string) string {
	return topicName + "_" + subName
}

func deadLetterCollectionName(topicName, subName string) string {
	return subscriptionCollectionName(topicName, subName) + "_deadletters"
}

// primary returns the collection name of db, always read from the primary,
// so that receivers never see stale lock states.
func primary(db *mongo.Database, name string) *mongo.Collection {
	return db.Collection(name, options.Collection().SetReadPreference(readpref.Primary()))
}

// registerSubscription registers the subscription subName to the topic
// topicName, so that the messages published to the topic from now on are
// delivered to it, and returns its collection. It is idempotent.
func registerSubscription(ctx context.Context, db *mongo.Database, topicName, subName string) (*mongo.Collection, error) {
	registry := primary(db, subscriptionsCollection)
	if _, err := registry.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "topic", Value: 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", subscriptionsCollection, err)
	}
	// The collection must exist before the first transaction inserts into it:
	// collections can't be created inside transactions before MongoDB 4.4.
	collName := subscriptionCollectionName(topicName, subName)
	if err := db.CreateCollection(ctx, collName); err != nil && !isNamespaceExists(err) {
		return nil, fmt.Errorf("creating the collection %s: %w", collName, err)
	}
	coll := primary(db, collName)
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "lockedUntil", Value: 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", collName, err)
	}
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "batchId", Value: 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", collName, err)
	}
	// The messages whose retention expired are found by their publish time.
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "publishTime", Value: 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", collName, err)
	}
	// Like the collection of the subscription, the one of its dead letters is
	// created before any transaction inserts into it.
	deadLettersName := deadLetterCollectionName(topicName, subName)
	if err := db.CreateCollection(ctx, deadLettersName); err != nil && !isNamespaceExists(err) {
		return nil, fmt.Errorf("creating the collection %s: %w", deadLettersName, err)
	}
	deadLetters := primary(db, deadLettersName)
	if _, err := deadLetters.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "deadLetterTime", Value: 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", deadLettersName, err)
	}
	// The subscription is registered last, so that publishers only see it once
	// its collection is ready.
	id := subscriptionID(topicName, subName)
	update := bson.D{{Key: "$setOnInsert", Value: subscriptionRegistration{
		ID:         id,
		Topic:      topicName,
		Collection: collName,
		CreatedAt:  time.Now(),
	}}}
	if _, err := registry.UpdateByID(ctx, id, update, options.UpdateOne().SetUpsert(true)); err != nil {
		if !mongo.IsDuplicateKeyError(err) { // Concurrent upsert of the same subscription.
			return nil, fmt.Errorf("registering the subscription %s: %w", id, err)
		}
	}
	return coll, nil
}

// DeleteSubscription unregisters the subscription subName of the topic
// topicName in db, and deletes its messages and its dead letters. The messages
// published to the topic aren't delivered to it anymore, unless it is opened
// again.
func DeleteSubscription(ctx context.Context, db *mongo.Database, topicName, subName string) error {
	id := subscriptionID(topicName, subName)
	if _, err := db.Collection(subscriptionsCollection).DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
		return fmt.Errorf("unregistering the subscription %s: %w", id, err)
	}
	if err := db.Collection(subscriptionCollectionName(topicName, subName)).Drop(ctx); err != nil {
		return fmt.Errorf("dropping the collection of the subscription %s: %w", id, err)
	}
	if err := db.Collection(deadLetterCollectionName(topicName, subName)).Drop(ctx); err != nil {
		return fmt.Errorf("dropping the dead letters of the subscription %s: %w", id, err)
	}
	return nil
}

func isNamespaceExists(err error) bool {
	var se mongo.ServerError
	return errors.As(err, &se) && se.HasErrorCode(48)
}

type topic struct {
	db   *mongo.Database
	name string
}

func newTopic(db *mongo.Database, name string) *topic {
	return &topic{db: db, name: name}
}

// OpenTopic opens the topic topicName, whose subscriptions are in db.
func OpenTopic(db *mongo.Database, topicName string) (*pubsub.Topic, error) {
	return pubsub.NewTopic(newTopic(db, topicName), nil), nil
}

var transactionOptions = options.Transaction().
	SetReadConcern(readconcern.Snapshot()).
	SetWriteConcern(writeconcern.Majority()).
	SetReadPreference(readpref.Primary())

func (t *topic) SendBatch(ctx context.Context, ms []*driver.Message) error {
	if t == nil {
		return errNotExist
	}
	now := time.Now()
	messages := make([]any, len(ms))
	for i, m := range ms {
		if m.BeforeSend != nil {
			if err := m.BeforeSend(func(any) bool { return false }); err != nil {
				return err
			}
		}
		md := make(map[string]string, len(m.Metadata))
		for k, v := range m.Metadata {
			k = escape.HexEscape(k, func(runes []rune, i int) bool {
				c := runes[i]
				return c <= 16
			})
			md[k] = v
		}
		messages[i] = message{
			ID:          bson.NewObjectID(),
			Version:     messageVersion,
			Body:        m.Body,
			Metadata:    md,
			PublishTime: now,
		}
	}

	session, err := t.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	// The subscriptions are read in the same transaction as the inserts, so that
	// the messages are either inserted into every subscription registered when
	// the transaction started, or into none of them. WithTransaction retries the
	// transient errors, such as a primary election.
	_, err = session.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		cursor, err := t.db.Collection(subscriptionsCollection).Find(ctx, bson.D{{Key: "topic", Value: t.name}})
		if err != nil {
			return nil, err
		}
		var subs []subscriptionRegistration
		if err := cursor.All(ctx, &subs); err != nil {
			return nil, err
		}
		for _, sub := range subs {
			if _, err := t.db.Collection(sub.Collection).InsertMany(ctx, messages); err != nil {
				return nil, fmt.Errorf("inserting into the subscription %s: %w", sub.ID, err)
			}
		}
		return nil, nil
	}, transactionOptions)
	if err != nil {
		return err
	}

	for _, m := range ms {
		if m.AfterSend != nil {
			if err := m.AfterSend(func(any) bool { return false }); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *topic) IsRetryable(err error) bool {
	return isRetryable(err)
}

func (t *topic) As(i any) bool {
	p, ok := i.(**mongo.Database)
	if !ok {
		return false
	}
	*p = t.db
	return true
}

func (t *topic) ErrorAs(err error, a any) bool {
	return errorAs(err, a)
}

func (t *topic) ErrorCode(err error) gcerrors.ErrorCode {
	return errorCode(err)
}

func (t *topic) Close() error {
	// The MongoDB client is shared and owned by the caller, so there is nothing to close.
	return nil
}

type subscription struct {
	id           string
	subscription *mongo.Collection
	deadLetters  *mongo.Collection
	ackDeadline  time.Duration
	minBackoff   time.Duration
	maxBackoff   time.Duration
	retention    time.Duration

	// lastDeadLettering is when the expired messages were last moved to the
	// dead letters, at most every deadLetteringInterval.
	mu                sync.Mutex
	lastDeadLettering time.Time
}

// DefaultRetention is how long a message is retained for delivery after it was
// published, when the subscription doesn't set a retention.
const DefaultRetention = 24 * time.Hour

// deadLetteringInterval is how often, at most, a subscription moves the
// messages whose retention expired to its dead letters, when it receives.
var deadLetteringInterval = time.Minute

// deadLetteringBatchSize is the maximum number of messages moved to the dead
// letters, or replayed, in a transaction.
const deadLetteringBatchSize = 100

// SubscriptionOptions are the options of a subscription.
type SubscriptionOptions struct {
	// AckDeadline is how long a received message stays locked: it is delivered
	// again if it isn't acked or nacked by then.
	AckDeadline time.Duration
	// MinBackoff is the delay before a nacked message is delivered again the
	// first time. It doubles with each delivery of the message, up to
	// MaxBackoff. Zero means that a nacked message is delivered again at once.
	MinBackoff time.Duration
	// MaxBackoff is the maximum delay before a nacked message is delivered
	// again. Zero means 10 times MinBackoff.
	MaxBackoff time.Duration
	// Retention is how long a message is retained for delivery after it was
	// published, whether it was delivered or not: a message still not acked by
	// then isn't delivered anymore, and is moved to the dead letters of the
	// subscription once it isn't locked anymore (by a receiver until its ack
	// deadline, or by the backoff after a nack), within a minute while something
	// receives from the subscription. A message whose ack deadline passed while
	// its receiver was still processing it can be moved too, its late ack then
	// having no effect. Zero means DefaultRetention.
	Retention time.Duration
}

func (o *SubscriptionOptions) validate() error {
	switch {
	case o.MinBackoff < 0 || o.MaxBackoff < 0:
		return errors.New("the backoff can't be negative")
	case o.Retention < 0:
		return errors.New("the retention can't be negative")
	case o.MinBackoff == 0 && o.MaxBackoff > 0:
		return errors.New("a maximum backoff requires a minimum backoff")
	case o.MaxBackoff > 0 && o.MaxBackoff < o.MinBackoff:
		return fmt.Errorf("the maximum backoff %s is less than the minimum backoff %s", o.MaxBackoff, o.MinBackoff)
	}
	return nil
}

func newSubscription(ctx context.Context, db *mongo.Database, topicName, subName string, opts *SubscriptionOptions) (*subscription, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if db == nil {
		return &subscription{}, nil
	}
	coll, err := registerSubscription(ctx, db, topicName, subName)
	if err != nil {
		return nil, err
	}
	maxBackoff := opts.MaxBackoff
	if maxBackoff == 0 {
		maxBackoff = 10 * opts.MinBackoff
	}
	retention := opts.Retention
	if retention == 0 {
		retention = DefaultRetention
	}
	return &subscription{
		id:           subscriptionID(topicName, subName),
		subscription: coll,
		deadLetters:  primary(db, deadLetterCollectionName(topicName, subName)),
		ackDeadline:  opts.AckDeadline,
		minBackoff:   opts.MinBackoff,
		maxBackoff:   maxBackoff,
		retention:    retention,
	}, nil
}

// OpenSubscription opens the subscription subName of the topic topicName, in db.
// The subscription is registered if it isn't yet: it receives the messages
// published to the topic from now on, even while it isn't open.
// A nacked message is delivered again at once: see OpenSubscriptionWithOptions
// for a backoff.
func OpenSubscription(ctx context.Context, db *mongo.Database, topicName, subName string, ackDeadline time.Duration) (*pubsub.Subscription, error) {
	return OpenSubscriptionWithOptions(ctx, db, topicName, subName, &SubscriptionOptions{AckDeadline: ackDeadline})
}

// OpenSubscriptionWithOptions is OpenSubscription with options.
func OpenSubscriptionWithOptions(ctx context.Context, db *mongo.Database, topicName, subName string, opts *SubscriptionOptions) (*pubsub.Subscription, error) {
	s, err := newSubscription(ctx, db, topicName, subName, opts)
	if err != nil {
		return nil, err
	}
	return pubsub.NewSubscription(s, nil, nil), nil
}

// pollInterval is how long ReceiveBatch waits when no message is available.
const pollInterval = 100 * time.Millisecond

func (s *subscription) ReceiveBatch(ctx context.Context, maxMessages int) ([]*driver.Message, error) {
	if s.subscription == nil {
		return nil, errNotExist
	}
	// A failure only delays the dead-lettering, which isn't a reason to stop
	// delivering the other messages.
	if err := s.deadLetterExpiredMessages(ctx); err != nil && ctx.Err() == nil {
		log.Printf("mongopubsub: moving the expired messages of the subscription %s to its dead letters: %v", s.id, err)
	}
	msgs, batchID, err := s.fetchAvailableMessages(ctx, maxMessages)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
		return nil, nil
	}
	dms := make([]*driver.Message, len(msgs))
	for i, msg := range msgs {
		md := make(map[string]string, len(msg.Metadata))
		for k, v := range msg.Metadata {
			md[escape.HexUnescape(k)] = v
		}
		dms[i] = &driver.Message{
			LoggableID: msg.ID.Hex(),
			Body:       msg.Body,
			Metadata:   md,
			AckID:      ackID{ID: msg.ID, BatchID: batchID, DeliveryAttempts: msg.DeliveryAttempts},
			AsFunc:     func(any) bool { return false },
		}
	}
	return dms, nil
}

// fetchAvailableMessages locks up to maxMessages messages that aren't locked,
// nor past their retention, and returns them with the ID of the batch they were
// locked in.
func (s *subscription) fetchAvailableMessages(ctx context.Context, maxMessages int) ([]message, bson.ObjectID, error) {
	now := time.Now()
	batchID := bson.NewObjectID()
	// A message past its retention isn't delivered anymore: left unlocked, it is
	// moved to the dead letters by the next deadLetterExpiredMessages.
	available := bson.D{
		{Key: "lockedUntil", Value: bson.D{{Key: "$lt", Value: now}}},
		{Key: "publishTime", Value: bson.D{{Key: "$gte", Value: now.Add(-s.retention)}}},
	}

	// 1. We find the messages that aren't locked.
	cursor, err := s.subscription.Find(ctx, available,
		options.Find().SetLimit(int64(maxMessages)).SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, batchID, err
	}
	var candidates []struct {
		ID bson.ObjectID `bson:"_id"`
	}
	if err := cursor.All(ctx, &candidates); err != nil {
		return nil, batchID, err
	}
	if len(candidates) == 0 {
		return nil, batchID, nil
	}
	ids := make([]bson.ObjectID, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
	}

	// 2. We lock them, unless another receiver locked them in the meantime.
	lockFilter := append(bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}, available...)
	lock := bson.D{
		{Key: "$set", Value: bson.D{{Key: "lockedUntil", Value: now.Add(s.ackDeadline)}, {Key: "batchId", Value: batchID}}},
		{Key: "$inc", Value: bson.D{{Key: "deliveryAttempts", Value: 1}}},
	}
	results, err := s.subscription.UpdateMany(ctx, lockFilter, lock)
	if err != nil {
		return nil, batchID, err
	}
	if results.ModifiedCount == 0 {
		return nil, batchID, nil
	}

	// 3. We read the messages that we locked.
	finalCursor, err := s.subscription.Find(ctx, bson.D{{Key: "batchId", Value: batchID}})
	if err != nil {
		return nil, batchID, err
	}
	var msgs []message
	if err := finalCursor.All(ctx, &msgs); err != nil {
		return nil, batchID, err
	}
	return msgs, batchID, nil
}

// ackID identifies a received message, and the batch it was locked in.
type ackID struct {
	ID      bson.ObjectID
	BatchID bson.ObjectID
	// DeliveryAttempts is how many times the message has been delivered, this delivery included.
	DeliveryAttempts int
}

func (s *subscription) SendAcks(ctx context.Context, ackIDs []driver.AckID) error {
	ids := make([]bson.ObjectID, len(ackIDs))
	for i, id := range ackIDs {
		ids[i] = id.(ackID).ID
	}
	// The messages are deleted even if their ack deadline expired, since they
	// have been processed.
	_, err := s.subscription.DeleteMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	return err
}

func (s *subscription) CanNack() bool {
	return true
}

func (s *subscription) SendNacks(ctx context.Context, ackIDs []driver.AckID) error {
	// A message is only unlocked if it is still locked by the batch it was
	// received in, so that a message whose ack deadline expired and that has been
	// received again isn't unlocked.
	//
	// Without backoff, the nacked messages are available again at once. With a
	// backoff, they stay locked for a delay depending on how many times they have
	// been delivered: the messages are grouped by this delay.
	now := time.Now()
	locksByUntil := map[time.Time]bson.A{}
	for _, id := range ackIDs {
		id := id.(ackID)
		var until time.Time
		if s.minBackoff > 0 {
			until = now.Add(s.backoff(id.DeliveryAttempts))
		}
		locksByUntil[until] = append(locksByUntil[until], bson.D{{Key: "_id", Value: id.ID}, {Key: "batchId", Value: id.BatchID}})
	}
	for until, locks := range locksByUntil {
		update := bson.D{{Key: "$set", Value: bson.D{{Key: "lockedUntil", Value: until}}}}
		if _, err := s.subscription.UpdateMany(ctx, bson.D{{Key: "$or", Value: locks}}, update); err != nil {
			return err
		}
	}
	return nil
}

// backoff returns how long a message nacked after its deliveryAttempts-th
// delivery stays locked: the minimum backoff after the first delivery, doubling
// with each delivery, up to the maximum backoff.
func (s *subscription) backoff(deliveryAttempts int) time.Duration {
	d := s.minBackoff
	for i := 1; i < deliveryAttempts && d < s.maxBackoff; i++ {
		d *= 2
	}
	return min(d, s.maxBackoff)
}

func (s *subscription) IsRetryable(err error) bool {
	return isRetryable(err)
}

func (s *subscription) As(i any) bool {
	p, ok := i.(**mongo.Collection)
	if !ok {
		return false
	}
	*p = s.subscription
	return true
}

func (s *subscription) ErrorAs(err error, a any) bool {
	return errorAs(err, a)
}

func (s *subscription) ErrorCode(err error) gcerrors.ErrorCode {
	return errorCode(err)
}

func (s *subscription) Close() error {
	// The MongoDB client is shared and owned by the caller, so there is nothing to close.
	return nil
}

// IsTransient reports whether err, or an error it wraps, is a transient error of MongoDB: a network error, no server
// selected in time, or an error of a replica set changing its primary or of a node shutting down. The Go CDK has no
// error code for it (gcerrors.Code returns Unknown), so callers use IsTransient to retry or report the error as
// temporary.
func IsTransient(err error) bool {
	return mongoerr.IsTransient(err)
}

// isRetryable reports whether err is transient, so that the operation can be retried, typically during a failover of
// the replica set. A server selection error isn't retried, although it's transient: the driver already waited for a
// server for the server selection timeout, and the Go CDK retries until the context of the operation is done, which,
// for the acks and the receives in the background, only happens once the subscription is shut down, and shutting it
// down waits for the pending acks: a shutdown while MongoDB can't be reached would never end, and the receive errors
// wouldn't be reported.
func isRetryable(err error) bool {
	var sse topology.ServerSelectionError
	if errors.As(err, &sse) {
		return false
	}
	return mongoerr.IsTransient(err)
}

func errorAs(err error, a any) bool {
	switch e := err.(type) {
	case mongo.CommandError:
		if p, ok := a.(*mongo.CommandError); ok {
			*p = e
			return true
		}
	case mongo.BulkWriteError:
		if p, ok := a.(*mongo.BulkWriteError); ok {
			*p = e
			return true
		}
	case mongo.BulkWriteException:
		if p, ok := a.(*mongo.BulkWriteException); ok {
			*p = e
			return true
		}
	}
	return false
}

func errorCode(err error) gcerrors.ErrorCode {
	if errors.Is(err, errNotExist) {
		return gcerrors.NotFound
	}
	if g, ok := errors.AsType[*gcerr.Error](err); ok {
		return g.Code
	}
	switch {
	case errors.Is(err, context.Canceled):
		return gcerrors.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return gcerrors.DeadlineExceeded
	}
	return gcerrors.Unknown
}

type message struct {
	ID          bson.ObjectID     `bson:"_id"`
	Version     int               `bson:"v"`
	Body        []byte            `bson:"body"`
	Metadata    map[string]string `bson:"metadata"`
	PublishTime time.Time         `bson:"publishTime"`
	LockedUntil time.Time         `bson:"lockedUntil"`
	// DeliveryAttempts is how many times the message has been delivered. The
	// messages stored before it existed don't have it: they count from 0.
	DeliveryAttempts int           `bson:"deliveryAttempts,omitempty"`
	BatchID          bson.ObjectID `bson:"batchId,omitempty"`
}

// deadLetterDoc is a message in the dead letters of a subscription: the fields
// of message, without its lock, and the time it was moved there. (Embedding
// message, which is unexported, wouldn't decode.)
type deadLetterDoc struct {
	ID               bson.ObjectID     `bson:"_id"`
	Version          int               `bson:"v"`
	Body             []byte            `bson:"body"`
	Metadata         map[string]string `bson:"metadata"`
	PublishTime      time.Time         `bson:"publishTime"`
	DeliveryAttempts int               `bson:"deliveryAttempts,omitempty"`
	DeadLetterTime   time.Time         `bson:"deadLetterTime"`
}
