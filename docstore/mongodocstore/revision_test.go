package mongodocstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gocloud.dev/docstore"
	"gocloud.dev/gcerrors"
)

// TestWriteErrors checks the error of each write that doesn't apply: a stale revision is FailedPrecondition, as the
// other drivers report it, and a missing document is NotFound. Each failing action gets its own error, the others
// succeed, and a failed write doesn't change the revision of its document.
func TestWriteErrors(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, serverURIV4)
	defer func() { _ = client.Disconnect(ctx) }()

	type doc struct {
		ID               string `docstore:"id"`
		Value            int
		DocstoreRevision interface{}
	}

	newColl := func(t *testing.T) *docstore.Collection {
		t.Helper()
		db := client.Database(fmt.Sprintf("%s-writes-%d", dbName, time.Now().UnixNano()))
		t.Cleanup(func() { _ = db.Drop(context.Background()) })
		dc, err := newCollection(db.Collection("docs"), "id", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		coll := docstore.NewCollection(dc)
		t.Cleanup(func() { _ = coll.Close() })
		return coll
	}
	// stale returns a copy of the document stored with id, then writes it again so that the copy's revision is stale.
	stale := func(t *testing.T, coll *docstore.Collection, id string) *doc {
		t.Helper()
		d := &doc{ID: id, Value: 1}
		if err := coll.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
		staleCopy := *d
		d.Value = 2
		if err := coll.Replace(ctx, d); err != nil {
			t.Fatal(err)
		}
		return &staleCopy
	}

	tests := map[string]struct {
		write    func(t *testing.T, coll *docstore.Collection) error
		wantCode gcerrors.ErrorCode
	}{
		"Replace with a stale revision": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return coll.Replace(ctx, stale(t, coll, "a"))
			},
			wantCode: gcerrors.FailedPrecondition,
		},
		"Put with a stale revision": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return coll.Put(ctx, stale(t, coll, "a"))
			},
			wantCode: gcerrors.FailedPrecondition,
		},
		"Update with a stale revision": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return coll.Update(ctx, stale(t, coll, "a"), docstore.Mods{"Value": 3})
			},
			wantCode: gcerrors.FailedPrecondition,
		},
		"Replace of a missing document": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return coll.Replace(ctx, &doc{ID: "missing", Value: 1})
			},
			wantCode: gcerrors.NotFound,
		},
		"atomic Replace with a stale revision": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return firstError(coll.Actions().Replace(stale(t, coll, "a")).AtomicWrites().Do(ctx))
			},
			wantCode: gcerrors.FailedPrecondition,
		},
		"atomic Put with a stale revision": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return firstError(coll.Actions().Put(stale(t, coll, "a")).AtomicWrites().Do(ctx))
			},
			wantCode: gcerrors.FailedPrecondition,
		},
		"atomic Replace of a missing document": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return firstError(coll.Actions().Replace(&doc{ID: "missing", Value: 1}).AtomicWrites().Do(ctx))
			},
			wantCode: gcerrors.NotFound,
		},
		"Update of a missing document": {
			write: func(t *testing.T, coll *docstore.Collection) error {
				return coll.Update(ctx, &doc{ID: "missing"}, docstore.Mods{"Value": 1})
			},
			wantCode: gcerrors.NotFound,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			coll := newColl(t)
			if err := tc.write(t, coll); gcerrors.Code(err) != tc.wantCode {
				t.Errorf("got the error %v (%v), want %v", err, gcerrors.Code(err), tc.wantCode)
			}
		})
	}

	t.Run("each action of a list gets its own error", func(t *testing.T) {
		coll := newColl(t)
		staleDoc := stale(t, coll, "stale")
		staleRev := staleDoc.DocstoreRevision
		okDoc := &doc{ID: "ok", Value: 1}
		if err := coll.Create(ctx, okDoc); err != nil {
			t.Fatal(err)
		}
		okDoc.Value = 2

		err := coll.Actions().
			Replace(staleDoc).
			Replace(&doc{ID: "missing", Value: 1}).
			Replace(okDoc).
			Do(ctx)

		alerr, ok := err.(docstore.ActionListError)
		if !ok {
			t.Fatalf("got the error %v (%T), want an ActionListError", err, err)
		}
		got := map[int]gcerrors.ErrorCode{}
		for _, e := range alerr {
			got[e.Index] = gcerrors.Code(e.Err)
		}
		want := map[int]gcerrors.ErrorCode{0: gcerrors.FailedPrecondition, 1: gcerrors.NotFound}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got the errors %v, want %v", got, want)
		}
		if staleDoc.DocstoreRevision != staleRev {
			t.Errorf("the revision of the failed Replace changed from %v to %v", staleRev, staleDoc.DocstoreRevision)
		}
		stored := &doc{ID: "ok"}
		if err := coll.Get(ctx, stored); err != nil {
			t.Fatal(err)
		}
		if stored.Value != 2 {
			t.Errorf("got the stored value %d, want the successful Replace applied", stored.Value)
		}
	})
}

// firstError returns the error of the first action of an ActionListError, or err itself.
func firstError(err error) error {
	if alerr, ok := err.(docstore.ActionListError); ok && len(alerr) > 0 {
		return alerr[0].Err
	}
	return err
}
