package mongodocstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/x/mongo/driver/topology"
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
		// The Go CDK wraps the errors of the driver in errors with an Unwrap() []error method, as errors.Join does.
		"network error wrapped as by the Go CDK":        {err: errors.Join(mongo.CommandError{Labels: []string{"NetworkError"}}), want: true},
		"primary stepped down wrapped as by the Go CDK": {err: errors.Join(mongo.CommandError{Code: 189}), want: true},
		"network timeout of another client":             {err: &net.OpError{Op: "dial", Err: timeoutError{}}, want: false},
		"other server error":                            {err: mongo.CommandError{Code: 2}, want: false},
		"duplicate key":                                 {err: mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000}}}, want: false},
		"server selection timeout":                      {err: topology.ServerSelectionError{Wrapped: context.DeadlineExceeded}, want: true},
		"server selection cancelled":                    {err: topology.ServerSelectionError{Wrapped: context.Canceled}, want: false},
		"context canceled":                              {err: context.Canceled, want: false},
		"context deadline exceeded":                     {err: fmt.Errorf("finding: %w", context.DeadlineExceeded), want: false},
		"other error":                                   {err: errors.New("boom"), want: false},
		"no error":                                      {err: nil, want: false},
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

// timeoutError is a net.Error timing out, as a network error of a client other than the MongoDB driver.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
