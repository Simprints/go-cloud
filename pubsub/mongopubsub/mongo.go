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
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/escape"
	"gocloud.dev/internal/gcerr"
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
// Query parameters:
//   - ackdeadline: The ack deadline for OpenSubscription, in time.ParseDuration formats.
//     Defaults to 5m.
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

	ackDeadline := 5 * time.Minute
	if s := q.Get("ackdeadline"); s != "" {
		var err error
		ackDeadline, err = time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("open subscription %v: invalid ackdeadline %q: %v", u, s, err)
		}
		q.Del("ackdeadline")
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
	return OpenSubscription(ctx, o.Client.Database(dbName), pathSplit[0], pathSplit[1], ackDeadline)
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
	if _, err := registry.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{"topic", 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", subscriptionsCollection, err)
	}
	// The collection must exist before the first transaction inserts into it:
	// collections can't be created inside transactions before MongoDB 4.4.
	collName := subscriptionCollectionName(topicName, subName)
	if err := db.CreateCollection(ctx, collName); err != nil && !isNamespaceExists(err) {
		return nil, fmt.Errorf("creating the collection %s: %w", collName, err)
	}
	coll := primary(db, collName)
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{"lockedUntil", 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", collName, err)
	}
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{"batchId", 1}}}); err != nil {
		return nil, fmt.Errorf("creating the index of %s: %w", collName, err)
	}
	// The subscription is registered last, so that publishers only see it once
	// its collection is ready.
	id := subscriptionID(topicName, subName)
	update := bson.D{{"$setOnInsert", subscriptionRegistration{
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
// topicName in db, and deletes its messages. The messages published to the
// topic aren't delivered to it anymore, unless it is opened again.
func DeleteSubscription(ctx context.Context, db *mongo.Database, topicName, subName string) error {
	id := subscriptionID(topicName, subName)
	if _, err := db.Collection(subscriptionsCollection).DeleteOne(ctx, bson.D{{"_id", id}}); err != nil {
		return fmt.Errorf("unregistering the subscription %s: %w", id, err)
	}
	if err := db.Collection(subscriptionCollectionName(topicName, subName)).Drop(ctx); err != nil {
		return fmt.Errorf("dropping the collection of the subscription %s: %w", id, err)
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
		cursor, err := t.db.Collection(subscriptionsCollection).Find(ctx, bson.D{{"topic", t.name}})
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
	subscription *mongo.Collection
	ackDeadline  time.Duration
}

func newSubscription(ctx context.Context, db *mongo.Database, topicName, subName string, ackDeadline time.Duration) (*subscription, error) {
	if db == nil {
		return &subscription{}, nil
	}
	coll, err := registerSubscription(ctx, db, topicName, subName)
	if err != nil {
		return nil, err
	}
	return &subscription{subscription: coll, ackDeadline: ackDeadline}, nil
}

// OpenSubscription opens the subscription subName of the topic topicName, in db.
// The subscription is registered if it isn't yet: it receives the messages
// published to the topic from now on, even while it isn't open.
func OpenSubscription(ctx context.Context, db *mongo.Database, topicName, subName string, ackDeadline time.Duration) (*pubsub.Subscription, error) {
	s, err := newSubscription(ctx, db, topicName, subName, ackDeadline)
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
			AckID:      ackID{ID: msg.ID, BatchID: batchID},
			AsFunc:     func(any) bool { return false },
		}
	}
	return dms, nil
}

// fetchAvailableMessages locks up to maxMessages messages that aren't locked,
// and returns them with the ID of the batch they were locked in.
func (s *subscription) fetchAvailableMessages(ctx context.Context, maxMessages int) ([]message, bson.ObjectID, error) {
	now := time.Now()
	batchID := bson.NewObjectID()
	available := bson.D{{"lockedUntil", bson.D{{"$lt", now}}}}

	// 1. We find the messages that aren't locked.
	cursor, err := s.subscription.Find(ctx, available,
		options.Find().SetLimit(int64(maxMessages)).SetProjection(bson.D{{"_id", 1}}))
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
	lockFilter := append(bson.D{{"_id", bson.D{{"$in", ids}}}}, available...)
	lock := bson.D{{"$set", bson.D{{"lockedUntil", now.Add(s.ackDeadline)}, {"batchId", batchID}}}}
	results, err := s.subscription.UpdateMany(ctx, lockFilter, lock)
	if err != nil {
		return nil, batchID, err
	}
	if results.ModifiedCount == 0 {
		return nil, batchID, nil
	}

	// 3. We read the messages that we locked.
	finalCursor, err := s.subscription.Find(ctx, bson.D{{"batchId", batchID}})
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
}

func (s *subscription) SendAcks(ctx context.Context, ackIDs []driver.AckID) error {
	ids := make([]bson.ObjectID, len(ackIDs))
	for i, id := range ackIDs {
		ids[i] = id.(ackID).ID
	}
	// The messages are deleted even if their ack deadline expired, since they
	// have been processed.
	_, err := s.subscription.DeleteMany(ctx, bson.D{{"_id", bson.D{{"$in", ids}}}})
	return err
}

func (s *subscription) CanNack() bool {
	return true
}

func (s *subscription) SendNacks(ctx context.Context, ackIDs []driver.AckID) error {
	// A message is only unlocked if it is still locked by the batch it was
	// received in, so that a message whose ack deadline expired and that has been
	// received again isn't unlocked.
	locks := make(bson.A, len(ackIDs))
	for i, id := range ackIDs {
		id := id.(ackID)
		locks[i] = bson.D{{"_id", id.ID}, {"batchId", id.BatchID}}
	}
	update := bson.D{{"$set", bson.D{{"lockedUntil", time.Time{}}}}}
	_, err := s.subscription.UpdateMany(ctx, bson.D{{"$or", locks}}, update)
	return err
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

// retryableCodes are the codes of the errors returned while the replica set
// changes its primary, or while a node shuts down.
var retryableCodes = []int{
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

// isRetryable reports whether err is transient, so that the operation can be
// retried, typically during a failover of the replica set.
func isRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if mongo.IsNetworkError(err) {
		return true
	}
	var se mongo.ServerError
	if !errors.As(err, &se) {
		return false
	}
	if se.HasErrorLabel("RetryableWriteError") || se.HasErrorLabel("TransientTransactionError") {
		return true
	}
	return slices.ContainsFunc(retryableCodes, se.HasErrorCode)
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
	BatchID     bson.ObjectID     `bson:"batchId,omitempty"`
}
