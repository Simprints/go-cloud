package mongodocstore

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestIsTransient_ServerDown checks that the error of an action on a server that can't be reached, as returned by the
// docstore API, is transient.
func TestIsTransient_ServerDown(t *testing.T) {
	ctx := context.Background()
	// Nothing listens on this port; the server selection gives up quickly.
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://localhost:1/?serverSelectionTimeoutMS=300&connectTimeoutMS=300"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(ctx) }()
	coll, err := OpenCollection(client.Database("db").Collection("coll"), "id", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer coll.Close()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = coll.Get(ctx, map[string]any{"id": "a"})
	if err == nil {
		t.Fatal("got no error, want the error of a server that can't be reached")
	}
	if !IsTransient(err) {
		t.Errorf("IsTransient(%v) = false, want true", err)
	}
}

// timeoutError is a net.Error timing out, as a network error of a client other than the MongoDB driver.
