package mongoerr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
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
