package mongopubsub

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"gocloud.dev/internal/escape"
)

// deadLetterExpiredMessages moves the messages of the subscription whose
// retention expired, and that aren't locked by a receiver, to its dead letters.
// It does so at most every deadLetteringInterval, by batches of
// deadLetteringBatchSize messages, each in a transaction.
func (s *subscription) deadLetterExpiredMessages(ctx context.Context) error {
	s.mu.Lock()
	if time.Since(s.lastDeadLettering) < deadLetteringInterval {
		s.mu.Unlock()
		return nil
	}
	s.lastDeadLettering = time.Now()
	s.mu.Unlock()

	total := 0
	defer func() {
		if total > 0 {
			log.Printf("mongopubsub: moved %d messages of the subscription %s to its dead letters, as they weren't acked within %s of their publication", total, s.id, s.retention)
		}
	}()
	for {
		n, err := s.deadLetterExpiredBatch(ctx)
		total += n
		if err != nil {
			return err
		}
		if n < deadLetteringBatchSize {
			return nil
		}
	}
}

// deadLetterExpiredBatch moves up to deadLetteringBatchSize expired messages to
// the dead letters, in a transaction: a message locked by a receiver in the
// meantime makes the transaction conflict, and WithTransaction retries it.
func (s *subscription) deadLetterExpiredBatch(ctx context.Context) (int, error) {
	session, err := s.subscription.Database().Client().StartSession()
	if err != nil {
		return 0, err
	}
	defer session.EndSession(ctx)
	n, err := session.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		now := time.Now()
		filter := bson.D{
			{Key: "publishTime", Value: bson.D{{Key: "$lt", Value: now.Add(-s.retention)}}},
			{Key: "lockedUntil", Value: bson.D{{Key: "$lt", Value: now}}},
		}
		cursor, err := s.subscription.Find(ctx, filter, options.Find().SetLimit(deadLetteringBatchSize))
		if err != nil {
			return 0, err
		}
		var msgs []message
		if err := cursor.All(ctx, &msgs); err != nil {
			return 0, err
		}
		if len(msgs) == 0 {
			return 0, nil
		}
		docs := make([]any, len(msgs))
		ids := make(bson.A, len(msgs))
		for i, m := range msgs {
			docs[i] = deadLetterDoc{
				ID:               m.ID,
				Version:          m.Version,
				Body:             m.Body,
				Metadata:         m.Metadata,
				PublishTime:      m.PublishTime,
				DeliveryAttempts: m.DeliveryAttempts,
				DeadLetterTime:   now,
			}
			ids[i] = m.ID
		}
		if _, err := s.deadLetters.InsertMany(ctx, docs); err != nil {
			return 0, err
		}
		if _, err := s.subscription.DeleteMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}); err != nil {
			return 0, err
		}
		return len(msgs), nil
	}, transactionOptions)
	if err != nil {
		return 0, err
	}
	return n.(int), nil
}

// DeadLetter is a message of a subscription that wasn't acked within its
// retention, and was moved to the dead letters of the subscription.
type DeadLetter struct {
	// ID identifies the message, to replay or delete it.
	ID       string
	Body     []byte
	Metadata map[string]string
	// PublishTime is when the message was published, or replayed last.
	PublishTime time.Time
	// DeadLetterTime is when the message was moved to the dead letters.
	DeadLetterTime time.Time
	// DeliveryAttempts is how many times the message was delivered before.
	DeliveryAttempts int
}

// ListDeadLetters returns the dead letters of the subscription subName of the
// topic topicName in db, the oldest first, up to limit of them (all of them
// when limit isn't positive).
func ListDeadLetters(ctx context.Context, db *mongo.Database, topicName, subName string, limit int) ([]DeadLetter, error) {
	opts := options.Find().SetSort(bson.D{{Key: "deadLetterTime", Value: 1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cursor, err := primary(db, deadLetterCollectionName(topicName, subName)).Find(ctx, bson.D{}, opts)
	if err != nil {
		return nil, fmt.Errorf("listing the dead letters of the subscription %s: %w", subscriptionID(topicName, subName), err)
	}
	var docs []deadLetterDoc
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("listing the dead letters of the subscription %s: %w", subscriptionID(topicName, subName), err)
	}
	letters := make([]DeadLetter, len(docs))
	for i, d := range docs {
		md := make(map[string]string, len(d.Metadata))
		for k, v := range d.Metadata {
			md[escape.HexUnescape(k)] = v
		}
		letters[i] = DeadLetter{
			ID:               d.ID.Hex(),
			Body:             d.Body,
			Metadata:         md,
			PublishTime:      d.PublishTime,
			DeadLetterTime:   d.DeadLetterTime,
			DeliveryAttempts: d.DeliveryAttempts,
		}
	}
	return letters, nil
}

// CountDeadLetters returns the number of dead letters of the subscription
// subName of the topic topicName in db.
func CountDeadLetters(ctx context.Context, db *mongo.Database, topicName, subName string) (int64, error) {
	n, err := primary(db, deadLetterCollectionName(topicName, subName)).CountDocuments(ctx, bson.D{})
	if err != nil {
		return 0, fmt.Errorf("counting the dead letters of the subscription %s: %w", subscriptionID(topicName, subName), err)
	}
	return n, nil
}

// ReplayDeadLetters moves the dead letters ids (all of them when ids is empty)
// of the subscription subName of the topic topicName in db back into the
// subscription, to be delivered again, as if they had just been published: their
// retention starts again, and they count their deliveries from zero. It returns
// how many it replayed. The subscription must be registered.
func ReplayDeadLetters(ctx context.Context, db *mongo.Database, topicName, subName string, ids []string) (int, error) {
	id := subscriptionID(topicName, subName)
	registered, err := primary(db, subscriptionsCollection).CountDocuments(ctx, bson.D{{Key: "_id", Value: id}})
	if err != nil {
		return 0, fmt.Errorf("finding the subscription %s: %w", id, err)
	}
	if registered == 0 {
		return 0, fmt.Errorf("replaying the dead letters of the subscription %s: %w", id, errNotExist)
	}
	return moveDeadLetters(ctx, db, topicName, subName, ids, func(ctx context.Context, docs []deadLetterDoc) error {
		now := time.Now()
		msgs := make([]any, len(docs))
		for i, d := range docs {
			msgs[i] = message{ID: d.ID, Version: messageVersion, Body: d.Body, Metadata: d.Metadata, PublishTime: now}
		}
		_, err := primary(db, subscriptionCollectionName(topicName, subName)).InsertMany(ctx, msgs)
		return err
	})
}

// DeleteDeadLetters deletes the dead letters ids (all of them when ids is
// empty) of the subscription subName of the topic topicName in db. It returns
// how many it deleted.
func DeleteDeadLetters(ctx context.Context, db *mongo.Database, topicName, subName string, ids []string) (int, error) {
	return moveDeadLetters(ctx, db, topicName, subName, ids, nil)
}

// moveDeadLetters removes the dead letters ids (all of them when ids is empty)
// by batches, each in a transaction which first calls move, when it isn't nil,
// with the dead letters of the batch.
func moveDeadLetters(ctx context.Context, db *mongo.Database, topicName, subName string, ids []string, move func(context.Context, []deadLetterDoc) error) (int, error) {
	subID := subscriptionID(topicName, subName)
	filter := bson.D{}
	if len(ids) > 0 {
		oids := make(bson.A, len(ids))
		for i, id := range ids {
			oid, err := bson.ObjectIDFromHex(id)
			if err != nil {
				return 0, fmt.Errorf("invalid ID %q of a dead letter of the subscription %s: %w", id, subID, err)
			}
			oids[i] = oid
		}
		filter = bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: oids}}}}
	}
	deadLetters := primary(db, deadLetterCollectionName(topicName, subName))
	session, err := db.Client().StartSession()
	if err != nil {
		return 0, err
	}
	defer session.EndSession(ctx)
	total := 0
	for {
		n, err := session.WithTransaction(ctx, func(ctx context.Context) (any, error) {
			cursor, err := deadLetters.Find(ctx, filter, options.Find().SetLimit(deadLetteringBatchSize))
			if err != nil {
				return 0, err
			}
			var docs []deadLetterDoc
			if err := cursor.All(ctx, &docs); err != nil {
				return 0, err
			}
			if len(docs) == 0 {
				return 0, nil
			}
			if move != nil {
				if err := move(ctx, docs); err != nil {
					return 0, err
				}
			}
			batchIDs := make(bson.A, len(docs))
			for i, d := range docs {
				batchIDs[i] = d.ID
			}
			if _, err := deadLetters.DeleteMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: batchIDs}}}}); err != nil {
				return 0, err
			}
			return len(docs), nil
		}, transactionOptions)
		if err != nil {
			return total, fmt.Errorf("removing the dead letters of the subscription %s: %w", subID, err)
		}
		total += n.(int)
		if n.(int) < deadLetteringBatchSize {
			return total, nil
		}
	}
}
