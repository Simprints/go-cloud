package mongoerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"
)

func TestIsTransient(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"network error":               {err: mongo.CommandError{Labels: []string{"NetworkError"}}, want: true},
		"primary stepped down":        {err: mongo.CommandError{Code: 189}, want: true},
		"not writable primary":        {err: mongo.CommandError{Code: 10107}, want: true},
		"node shutting down":          {err: mongo.CommandError{Code: 91}, want: true},
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

// timeoutError is a net.Error timing out, as a network error of a client other than the MongoDB driver.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
