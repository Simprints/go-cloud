package mongodocstore

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestBSONDateTimeToTime(t *testing.T) {
	tests := map[string]time.Time{
		"a time":             time.Date(2026, 11, 19, 13, 4, 5, 123_000_000, time.UTC),
		"the zero time":      {},
		"before Unix epoch":  time.Date(1969, 12, 31, 23, 59, 59, 999_000_000, time.UTC),
		"in another zone":    time.Date(2026, 6, 1, 8, 0, 0, 0, time.FixedZone("EAT", 3*60*60)),
		"with sub-ms digits": time.Date(2026, 1, 2, 3, 4, 5, 678_901_234, time.UTC),
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			got := bsonDateTimeToTime(primitive.NewDateTimeFromTime(want))

			if got.Location() != time.UTC {
				t.Errorf("got the location %v, want UTC", got.Location())
			}
			if wantMs := want.Truncate(time.Millisecond); !got.Equal(wantMs) {
				t.Errorf("got %v, want %v", got, wantMs)
			}
			if got.IsZero() != want.IsZero() {
				t.Errorf("got IsZero %v, want %v", got.IsZero(), want.IsZero())
			}
		})
	}
}
