package mongoblob

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestIsTransient_ServerDown checks that the error of a read from a server that can't be reached, as returned by the
// blob API, is transient.
func TestIsTransient_ServerDown(t *testing.T) {
	// Nothing listens on this port; the server selection gives up quickly.
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://localhost:1/?serverSelectionTimeoutMS=300&connectTimeoutMS=300"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	bucket, err := OpenBucket(client.Database("db").GridFSBucket())
	if err != nil {
		t.Fatal(err)
	}
	defer bucket.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = bucket.ReadAll(ctx, "key")
	if err == nil {
		t.Fatal("got no error, want the error of a server that can't be reached")
	}
	if !IsTransient(err) {
		t.Errorf("IsTransient(%v) = false, want true", err)
	}
}
