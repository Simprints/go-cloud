package mongopubsub

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
	err            error
}

func (o *defaultDialer) OpenTopicURL(ctx context.Context, u *url.URL) (*pubsub.Topic, error) {
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
	return o.opener.OpenTopicURL(ctx, u)
}

func (o *defaultDialer) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*pubsub.Subscription, error) {
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
	return o.opener.OpenSubscriptionURL(ctx, u)
}

// Scheme is the URL scheme mongoblob registers its URLOpener under on
// docstore.DefaultMux.
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
		return nil, fmt.Errorf("open bucket %s: URL must have a non-empty Host (database name)", u)
	}
	topicName := strings.TrimPrefix(u.Path, "/")
	if topicName == "" {
		return nil, fmt.Errorf("open topic %s: URL must have a non-empty Path (topic name)", u)
	}
	return OpenTopic(o.Client.Database(dbName).Collection(topicName))
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
	if len(pathSplit) != 2 {
		return nil, fmt.Errorf("open subscription %s: URL path must be in the format /topic/subscription", u)
	}
	subscriptionColl := o.Client.Database(dbName).Collection(fmt.Sprintf("%s_%s", pathSplit[0], pathSplit[1]))
	topicColl := o.Client.Database(dbName).Collection(pathSplit[0])
	return OpenSubscription(ctx, topicColl, subscriptionColl, ackDeadline)
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

type topic struct {
	topic *mongo.Collection
}

func newTopic(coll *mongo.Collection) *topic {
	return &topic{topic: coll}
}

// OpenTopic opens a MongoDB bucket for use with Blob.
func OpenTopic(coll *mongo.Collection) (*pubsub.Topic, error) {
	return pubsub.NewTopic(newTopic(coll), nil), nil
}

func (t *topic) SendBatch(ctx context.Context, ms []*driver.Message) error {
	if t == nil {
		return errNotExist
	}
	messages := make([]message, len(ms))
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
		messages[i] = message{ID: bson.NewObjectID(), Body: m.Body, Metadata: md}
	}
	if _, err := t.topic.InsertMany(ctx, messages); err != nil {
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

func (t *topic) IsRetryable(_ error) bool {
	return false
}

func (t *topic) As(i any) bool {
	p, ok := i.(**mongo.Collection)
	if !ok {
		return false
	}
	*p = t.topic
	return true
}

func (t *topic) ErrorAs(err error, a any) bool {
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

func (t *topic) ErrorCode(err error) gcerrors.ErrorCode {
	if errors.Is(err, errNotExist) {
		return gcerrors.NotFound
	}
	var g *gcerr.Error
	if errors.As(err, &g) {
		return g.Code
	}
	return gcerrors.Unknown
}

func (t *topic) Close() error {
	// MongoDB Go driver does not have a method to close a collection, so we do nothing here.
	return nil
}

type subscription struct {
	topic        *mongo.Collection
	subscription *mongo.Collection
	copyStream   *mongo.ChangeStream
	ackDeadline  time.Duration

	receiveErr error

	wg     sync.WaitGroup
	cancel func()
	ctx    context.Context
}

func newSubscription(ctx context.Context, topic, sub *mongo.Collection, ackDeadline time.Duration) (*subscription, error) {
	if topic == nil {
		return &subscription{}, nil
	}
	streamCtx, cancel := context.WithCancel(ctx)
	copyStream, err := topic.Watch(streamCtx, mongo.Pipeline{bson.D{{"$match", bson.D{{"operationType", "insert"}}}}})
	if err != nil {
		cancel()
		return nil, err
	}
	s := &subscription{
		topic:        topic,
		subscription: sub,
		copyStream:   copyStream,
		ackDeadline:  ackDeadline,
		cancel:       cancel,
		ctx:          ctx,
	}
	s.wg.Add(1)
	go func() {
		fmt.Println("Start receiving change stream events")
		for s.copyStream.Next(streamCtx) {
			var event insertEvent[message]
			fmt.Println("Received change stream event")
			if err := s.copyStream.Decode(&event); err != nil {
				s.receiveErr = err
				continue
			}
			if _, err := s.subscription.InsertOne(streamCtx, event.FullDocument); err != nil {
				if !mongo.IsDuplicateKeyError(err) {
					s.receiveErr = err
					continue
				}
			}
		}
		s.wg.Done()
	}()
	return s, nil
}

func OpenSubscription(ctx context.Context, topic, sub *mongo.Collection, ackDeadline time.Duration) (*pubsub.Subscription, error) {
	s, err := newSubscription(ctx, topic, sub, ackDeadline)
	if err != nil {
		return nil, err
	}
	return pubsub.NewSubscription(s, nil, nil), nil
}

func (s *subscription) ReceiveBatch(ctx context.Context, maxMessages int) ([]*driver.Message, error) {
	if s.subscription == nil {
		return nil, errNotExist
	}
	if err := s.receiveErr; err != nil {
		s.receiveErr = nil
		return nil, err
	}
	msgs, err := s.fetchAvailableMessages(ctx, maxMessages)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		time.Sleep(100 * time.Millisecond)
		return nil, nil
	}
	dms := make([]*driver.Message, len(msgs))
	for i, msg := range msgs {
		md := make(map[string]string, len(msg.Metadata))
		for k, v := range msg.Metadata {
			md[escape.HexUnescape(k)] = v
		}
		dms[i] = &driver.Message{
			LoggableID: msg.ID.String(),
			Body:       msg.Body,
			Metadata:   md,
			AckID:      msg.ID,
			AsFunc:     func(any) bool { return false },
		}
	}
	return dms, nil
}

func (s *subscription) fetchAvailableMessages(ctx context.Context, maxMessages int) ([]message, error) {
	now := time.Now()
	batchID := bson.NewObjectID()

	// 1. We fetch the available messages that are not locked.
	filter := bson.D{{"lockedUntil", bson.D{{"$lt", now}}}}
	cursor, err := s.subscription.Find(ctx, filter, options.Find().SetLimit(int64(maxMessages)))
	if err != nil {
		return nil, err
	}
	var msgs []message
	if err := cursor.All(ctx, &msgs); err != nil {
		return nil, err
	}
	ids := make([]bson.ObjectID, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	// 2. We update the previous messages to lock them and only if they are not locked yet.
	updateFilter := bson.D{{"_id", bson.D{{"$in", ids}}}, {"lockedUntil", bson.D{{"$lt", now}}}}
	update := bson.D{{"$set", bson.D{{"lockedUntil", now.Add(s.ackDeadline)}, {"batchId", batchID}}}}
	results, err := s.subscription.UpdateMany(ctx, updateFilter, update)
	if err != nil {
		return nil, err
	}
	// 3.a If they were no concurrent updates we can return the messages.
	if int(results.ModifiedCount) == len(msgs) {
		return msgs, nil
	}
	// 3.b  If they were concurrent updates we checked which messages have been updated.
	finalCursor, err := s.subscription.Find(ctx, bson.D{{"batch_id", batchID}})
	if err != nil {
		return nil, err
	}
	var finalMsgs []message
	if err := finalCursor.All(ctx, &finalMsgs); err != nil {
		return nil, err
	}
	return finalMsgs, err
}

func (s *subscription) SendAcks(ctx context.Context, ackIDs []driver.AckID) error {
	filter := bson.D{{"_id", bson.D{{"$in", ackIDs}}}}
	if _, err := s.subscription.DeleteMany(ctx, filter); err != nil {
		return err
	}
	return nil
}

func (s *subscription) CanNack() bool {
	return true
}

func (s *subscription) SendNacks(ctx context.Context, ackIDs []driver.AckID) error {
	filter := bson.D{{"_id", bson.D{{"$in", ackIDs}}}}
	update := bson.D{{"$set", bson.D{{"lockedUntil", time.Time{}}}}}
	if _, err := s.subscription.UpdateMany(ctx, filter, update); err != nil {
		return err
	}
	return nil
}

func (s *subscription) IsRetryable(error) bool {
	return false
}

func (s *subscription) As(i any) bool {
	p, ok := i.(**mongo.Collection)
	if !ok {
		return false
	}
	*p = s.topic
	return true
}

func (s *subscription) ErrorAs(err error, a any) bool {
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

func (s *subscription) ErrorCode(err error) gcerrors.ErrorCode {
	if errors.Is(err, errNotExist) {
		return gcerrors.NotFound
	}
	var g *gcerr.Error
	if errors.As(err, &g) {
		return g.Code
	}
	return gcerrors.Unknown
}

func (s *subscription) Close() error {
	if s.copyStream == nil {
		return nil
	}
	s.cancel()
	s.wg.Wait()
	if err := s.copyStream.Close(s.ctx); err != nil {
		return err
	}
	return nil
}

type message struct {
	ID          bson.ObjectID     `bson:"_id"`
	Body        []byte            `bson:"body"`
	Metadata    map[string]string `bson:"metadata"`
	LockedUntil time.Time         `bson:"lockedUntil"`
	BatchID     bson.ObjectID     `bson:"batchId,omitempty"`
}

type insertEvent[T any] struct {
	ID           any `bson:"_id"`
	FullDocument T   `bson:"fullDocument"`
}
