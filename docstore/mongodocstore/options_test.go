package mongodocstore

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"

	"gocloud.dev/docstore"
)

// TestCollectionsDontShareOptions checks that each collection has its own options: the ones given to OpenCollection
// and the ones of a URLOpener are not changed, and the revision field of a collection opened from a URL doesn't change
// the one of the collections opened before, even when they are opened concurrently.
func TestCollectionsDontShareOptions(t *testing.T) {
	ctx := context.Background()
	// The client doesn't connect until it is used, which opening a collection doesn't do.
	client, err := Dial(ctx, "mongodb://localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	t.Run("OpenCollection doesn't change the given options", func(t *testing.T) {
		opts := &Options{}
		if _, err := newCollection(client.Database("db").Collection("coll"), "id", nil, opts); err != nil {
			t.Fatal(err)
		}
		if opts.RevisionField != "" {
			t.Errorf("got the revision field %q in the given options, want them unchanged", opts.RevisionField)
		}
	})

	t.Run("the revision field of a URL is the one of its collection only", func(t *testing.T) {
		opener := &URLOpener{Client: client}
		first := mustOpenCollectionURL(t, opener, "mongo://db/first?revision_field=FirstRevision")
		second := mustOpenCollectionURL(t, opener, "mongo://db/second?revision_field=SecondRevision")
		third := mustOpenCollectionURL(t, opener, "mongo://db/third")

		for coll, want := range map[*collection]string{
			first:  "FirstRevision",
			second: "SecondRevision",
			third:  docstore.DefaultRevisionField,
		} {
			if got := coll.RevisionField(); got != want {
				t.Errorf("got the revision field %q, want %q", got, want)
			}
		}
		if opener.Options.RevisionField != "" {
			t.Errorf("got the revision field %q in the options of the opener, want them unchanged",
				opener.Options.RevisionField)
		}
	})

	t.Run("collections can be opened concurrently from the same URLOpener", func(t *testing.T) {
		opener := &URLOpener{Client: client}
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Go(func() {
				coll := mustOpenCollectionURL(t, opener, fmt.Sprintf("mongo://db/coll%d?revision_field=Rev%d", i, i))
				if coll == nil {
					return
				}
				if got, want := coll.RevisionField(), fmt.Sprintf("Rev%d", i); got != want {
					t.Errorf("got the revision field %q, want %q", got, want)
				}
			})
		}
		wg.Wait()
	})
}

func mustOpenCollectionURL(t *testing.T, opener *URLOpener, rawURL string) *collection {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Error(err)
		return nil
	}
	coll, err := opener.newCollectionFromURL(u)
	if err != nil {
		t.Error(err)
		return nil
	}
	return coll
}
