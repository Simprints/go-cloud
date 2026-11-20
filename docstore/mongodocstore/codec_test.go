package mongodocstore

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.mongodb.org/mongo-driver/v2/bson"
	"gocloud.dev/docstore/drivertest"
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
			got := bsonDateTimeToTime(bson.NewDateTimeFromTime(want))

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

// TestDecodeDoc_EmbeddedDocuments decodes the embedded documents as the version 2 of the driver returns them: bson.D by
// default, bson.M or map[string]interface{} with the DefaultDocumentM or DefaultDocumentMap BSON options of the client.
func TestDecodeDoc_EmbeddedDocuments(t *testing.T) {
	type inner struct{ N int }
	type doc struct {
		ID    string `docstore:"_id"`
		In    inner
		List  []inner
		Other map[string]interface{}
	}
	want := doc{ID: "a", In: inner{N: 1}, List: []inner{{N: 2}}, Other: map[string]interface{}{"m": map[string]interface{}{"n": int64(3)}}}
	tests := map[string]func(kv ...bson.E) interface{}{
		"bson.D": func(kv ...bson.E) interface{} { return bson.D(kv) },
		"bson.M": func(kv ...bson.E) interface{} {
			m := bson.M{}
			for _, e := range kv {
				m[e.Key] = e.Value
			}
			return m
		},
		"map": func(kv ...bson.E) interface{} {
			m := map[string]interface{}{}
			for _, e := range kv {
				m[e.Key] = e.Value
			}
			return m
		},
	}
	for name, embedded := range tests {
		t.Run(name, func(t *testing.T) {
			m := map[string]interface{}{
				"_id":   "a",
				"In":    embedded(bson.E{Key: "N", Value: int64(1)}),
				"List":  bson.A{embedded(bson.E{Key: "N", Value: int64(2)})},
				"Other": embedded(bson.E{Key: "m", Value: embedded(bson.E{Key: "n", Value: int64(3)})}),
			}
			var got doc
			if err := decodeDoc(m, drivertest.MustDocument(&got), mongoIDField, false); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("decoded document mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
