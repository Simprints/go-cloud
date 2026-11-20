package mongodocstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestIsTransient(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"network error":               {err: mongo.CommandError{Labels: []string{"NetworkError"}}, want: true},
		"primary stepped down":        {err: mongo.CommandError{Code: 189}, want: true},
		"not writable primary":        {err: mongo.CommandError{Code: 10107}, want: true},
		"retryable write label":       {err: mongo.CommandError{Code: 1, Labels: []string{"RetryableWriteError"}}, want: true},
		"transient transaction label": {err: mongo.CommandError{Code: 1, Labels: []string{"TransientTransactionError"}}, want: true},
		"wrapped transient error":     {err: fmt.Errorf("finding: %w", mongo.CommandError{Code: 189}), want: true},
		"other server error":          {err: mongo.CommandError{Code: 2}, want: false},
		"duplicate key":               {err: mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000}}}, want: false},
		"context canceled":            {err: context.Canceled, want: false},
		"context deadline exceeded":   {err: fmt.Errorf("finding: %w", context.DeadlineExceeded), want: false},
		"other error":                 {err: errors.New("boom"), want: false},
		"no error":                    {err: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsTransient_ServerDown checks that the error of an action on a server that can't be reached, as returned by the
// docstore API, is transient.
func TestIsTransient_ServerDown(t *testing.T) {
	ctx := context.Background()
	// Nothing listens on this port; the server selection gives up quickly.
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:1/?serverSelectionTimeoutMS=300&connectTimeoutMS=300"))
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
