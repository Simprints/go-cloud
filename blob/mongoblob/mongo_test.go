package mongoblob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/blob/drivertest"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/testing/setup"
)

const (
	serverURIV4 = "mongodb://localhost:27017/?directConnection=true"
	dbName      = "blob-test"
	bucketName1 = "docstore-test-1"
)

type harness struct {
	db *mongo.Database
}

func (h *harness) MakeDriver(_ context.Context) (driver.Bucket, error) {
	return newBucket(h.db.GridFSBucket(options.GridFSBucket().SetName(bucketName1))), nil
}

func (h *harness) MakeDriverForNonexistentBucket(_ context.Context) (driver.Bucket, error) {
	return nil, nil
}

func (h *harness) HTTPClient() *http.Client {
	return nil
}

func (h *harness) SupportsCopy() bool {
	return true
}

func (h *harness) SupportsNonSlashDelimiters() bool {
	return true
}

func (h *harness) Close() {}

func TestConformance(t *testing.T) {
	t.Run("V4", func(t *testing.T) { testConformance(t, serverURIV4) })
}

func testConformance(t *testing.T, serverURI string) {
	client := newTestClient(t, serverURI)
	defer func() { _ = client.Disconnect(context.Background()) }()

	newHarness := func(context.Context, *testing.T) (drivertest.Harness, error) {
		return &harness{client.Database(fmt.Sprintf("%s-%d", dbName, time.Now().UnixNano()))}, nil
	}
	drivertest.RunConformanceTests(t, newHarness, nil)
}

func newTestClient(t *testing.T, serverURI string) *mongo.Client {
	if !setup.HasDockerTestEnvironment() {
		t.Skip("Skipping Mongo tests since the Mongo server is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := Dial(serverURI)
	if err != nil {
		t.Fatalf("dialing to %s: %v", serverURI, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("connecting to %s: %v", serverURI, err)
	}
	return client
}

// TestList checks the listings the conformance tests don't make: without a prefix, with a prefix holding regular
// expression characters, and over several pages with a prefix.
func TestList(t *testing.T) {
	client := newTestClient(t, serverURIV4)
	defer func() { _ = client.Disconnect(context.Background()) }()
	ctx := context.Background()

	db := client.Database(fmt.Sprintf("%s-list-%d", dbName, time.Now().UnixNano()))
	defer func() { _ = db.Drop(context.Background()) }()
	b, err := OpenBucket(db.GridFSBucket(options.GridFSBucket().SetName(bucketName1)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	keys := []string{"a.b/1", "a.b/2", "a.b/3", "axb/1", "c/1"}
	for _, key := range keys {
		if err := b.WriteAll(ctx, key, []byte(key), nil); err != nil {
			t.Fatalf("writing %s: %v", key, err)
		}
	}

	tests := map[string]struct {
		prefix   string
		pageSize int
		want     []string
	}{
		"no prefix":                      {want: keys},
		"no prefix, several pages":       {pageSize: 2, want: keys},
		"prefix with a regexp character": {prefix: "a.b/", want: []string{"a.b/1", "a.b/2", "a.b/3"}},
		"prefix over several pages":      {prefix: "a.b/", pageSize: 1, want: []string{"a.b/1", "a.b/2", "a.b/3"}},
		"prefix matching no key":         {prefix: "z", want: nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var got []string
			opts := &blob.ListOptions{Prefix: tc.prefix}
			if tc.pageSize == 0 {
				iter := b.List(opts)
				for {
					obj, err := iter.Next(ctx)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatalf("listing: %v", err)
					}
					got = append(got, obj.Key)
				}
			} else {
				token := blob.FirstPageToken
				for {
					objs, next, err := b.ListPage(ctx, token, tc.pageSize, opts)
					if err != nil {
						t.Fatalf("listing a page: %v", err)
					}
					for _, obj := range objs {
						got = append(got, obj.Key)
					}
					if len(next) == 0 {
						break
					}
					token = next
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWriteIfNotExist checks that a write refused because the blob already exists leaves the existing blob untouched,
// whether the refused content is written or not, and whatever its size and content type.
func TestWriteIfNotExist(t *testing.T) {
	client := newTestClient(t, serverURIV4)
	defer func() { _ = client.Disconnect(context.Background()) }()
	ctx := context.Background()

	db := client.Database(fmt.Sprintf("%s-ifnotexist-%d", dbName, time.Now().UnixNano()))
	defer func() { _ = db.Drop(context.Background()) }()
	b, err := OpenBucket(db.GridFSBucket(options.GridFSBucket().SetName(bucketName1)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	tests := map[string]struct {
		content     []byte
		contentType string
	}{
		"small content with a content type":  {content: []byte("new"), contentType: "text/plain"},
		"small content without content type": {content: []byte("new")},
		"large content without content type": {content: bytes.Repeat([]byte("n"), 1024)},
		"no content with a content type":     {contentType: "text/plain"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			key := "key-" + strings.ReplaceAll(name, " ", "-")
			if err := b.WriteAll(ctx, key, []byte("existing"), nil); err != nil {
				t.Fatalf("writing the existing blob: %v", err)
			}

			w, err := b.NewWriter(ctx, key, &blob.WriterOptions{IfNotExist: true, ContentType: tc.contentType})
			if err != nil {
				t.Fatalf("opening the writer: %v", err)
			}
			_, writeErr := w.Write(tc.content)
			closeErr := w.Close()
			if gcerrors.Code(writeErr) != gcerrors.FailedPrecondition && gcerrors.Code(closeErr) != gcerrors.FailedPrecondition {
				t.Errorf("got the errors %v (Write) and %v (Close), want FailedPrecondition", writeErr, closeErr)
			}

			got, err := b.ReadAll(ctx, key)
			if err != nil {
				t.Fatalf("reading the existing blob: %v", err)
			}
			if string(got) != "existing" {
				t.Errorf("got the content %q, want the existing blob kept", got)
			}
		})
	}
}

// TestWriteCancelled checks that a write aborted by cancelling its context stores no blob, keeps the existing one and
// leaves no chunk without a file, also for a body over the 16 MiB the upload stream buffers before inserting chunks.
func TestWriteCancelled(t *testing.T) {
	client := newTestClient(t, serverURIV4)
	defer func() { _ = client.Disconnect(context.Background()) }()
	ctx := context.Background()

	db := client.Database(fmt.Sprintf("%s-cancelled-%d", dbName, time.Now().UnixNano()))
	defer func() { _ = db.Drop(context.Background()) }()
	b, err := OpenBucket(db.GridFSBucket(options.GridFSBucket().SetName(bucketName1)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	tests := map[string]struct {
		size     int
		existing bool
	}{
		"small body":                      {size: 1024},
		"body over 16 MiB":                {size: 17 << 20},
		"body over 16 MiB with a blob":    {size: 17 << 20, existing: true},
		"small body with a blob":          {size: 1024, existing: true},
		"body over 32 MiB without a blob": {size: 33 << 20},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			key := "key-" + strings.ReplaceAll(name, " ", "-")
			if tc.existing {
				if err := b.WriteAll(ctx, key, []byte("existing"), nil); err != nil {
					t.Fatalf("writing the existing blob: %v", err)
				}
			}

			writeCtx, cancel := context.WithCancel(ctx)
			w, err := b.NewWriter(writeCtx, key, nil)
			if err != nil {
				t.Fatalf("opening the writer: %v", err)
			}
			if _, err := w.Write(bytes.Repeat([]byte("n"), tc.size)); err != nil {
				t.Fatalf("writing: %v", err)
			}
			cancel()
			if err := w.Close(); err == nil {
				t.Error("got no error closing the cancelled writer")
			}

			got, err := b.ReadAll(ctx, key)
			switch {
			case tc.existing && err != nil:
				t.Fatalf("reading the existing blob: %v", err)
			case tc.existing && string(got) != "existing":
				t.Errorf("got the content %q, want the existing blob kept", got)
			case !tc.existing && gcerrors.Code(err) != gcerrors.NotFound:
				t.Errorf("got the error %v reading the blob, want NotFound", err)
			}
			assertNoOrphanChunks(t, db)
		})
	}
}

// assertNoOrphanChunks fails the test when the bucket has chunks of no file.
func assertNoOrphanChunks(t *testing.T, db *mongo.Database) {
	t.Helper()
	ctx := context.Background()
	cursor, err := db.Collection(bucketName1+".files").Find(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	var files []struct {
		ID any `bson:"_id"`
	}
	if err := cursor.All(ctx, &files); err != nil {
		t.Fatal(err)
	}
	fileIDs := bson.A{}
	for _, f := range files {
		fileIDs = append(fileIDs, f.ID)
	}
	n, err := db.Collection(bucketName1+".chunks").CountDocuments(ctx, bson.D{{Key: "files_id", Value: bson.D{{Key: "$nin", Value: fileIDs}}}})
	if err != nil {
		t.Fatal(err)
	}
	if n > 0 {
		t.Errorf("got %d chunks without a file, want none", n)
	}
}

// TestWriteFailed checks that a write the Go CDK aborts without cancelling its context by the caller stores no blob,
// keeps the existing one and leaves no chunk without a file: a ContentMD5 mismatch, and a Write whose flush of 16 MiB
// fails with a transient error (closing the stream would store a blob missing these chunks, and delete the existing
// one).
func TestWriteFailed(t *testing.T) {
	client := newTestClient(t, serverURIV4)
	defer func() { _ = client.Disconnect(context.Background()) }()
	ctx := context.Background()

	tests := map[string]struct {
		opts          *blob.WriterOptions
		rejectChunks  bool
		wantWriteFail bool
	}{
		"ContentMD5 mismatch": {opts: &blob.WriterOptions{ContentMD5: []byte("0123456789abcdef")}},
		"failed flush":        {rejectChunks: true, wantWriteFail: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			db := client.Database(fmt.Sprintf("%s-failed-%d", dbName, time.Now().UnixNano()))
			defer func() { _ = db.Drop(context.Background()) }()
			b, err := OpenBucket(db.GridFSBucket(options.GridFSBucket().SetName(bucketName1)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = b.Close() }()
			if err := b.WriteAll(ctx, "key", []byte("existing"), nil); err != nil {
				t.Fatalf("writing the existing blob: %v", err)
			}
			if tc.rejectChunks {
				// Every new chunk is refused: chunk numbers are never negative.
				cmd := bson.D{{Key: "collMod", Value: bucketName1 + ".chunks"}, {Key: "validator", Value: bson.D{{Key: "n", Value: bson.D{{Key: "$lt", Value: 0}}}}}}
				if err := db.RunCommand(ctx, cmd).Err(); err != nil {
					t.Fatalf("adding the validator: %v", err)
				}
			}

			w, err := b.NewWriter(ctx, "key", tc.opts)
			if err != nil {
				t.Fatalf("opening the writer: %v", err)
			}
			_, writeErr := w.Write(bytes.Repeat([]byte("n"), 17<<20))
			if tc.wantWriteFail && writeErr == nil {
				t.Fatal("got no error writing, want the flush to fail")
			}
			if tc.rejectChunks {
				// The error was transient: the chunks are accepted again when the writer is closed.
				cmd := bson.D{{Key: "collMod", Value: bucketName1 + ".chunks"}, {Key: "validator", Value: bson.D{}}}
				if err := db.RunCommand(ctx, cmd).Err(); err != nil {
					t.Fatalf("removing the validator: %v", err)
				}
			}
			if err := w.Close(); err == nil {
				t.Error("got no error closing the failed writer")
			}

			got, err := b.ReadAll(ctx, "key")
			if err != nil {
				t.Fatalf("reading the existing blob: %v", err)
			}
			if string(got) != "existing" {
				t.Errorf("got %d bytes, want the existing blob kept", len(got))
			}
			assertNoOrphanChunks(t, db)
		})
	}
}

// TestDeleteInterrupted checks that a deletion failing before the chunks of the blob are deleted keeps the blob, so
// that a retry finds it and deletes it, rather than leaving its chunks without a file. MongoDB refuses to delete from a
// view, so replacing the chunks collection with a view of it makes their deletion fail.
func TestDeleteInterrupted(t *testing.T) {
	client := newTestClient(t, serverURIV4)
	defer func() { _ = client.Disconnect(context.Background()) }()
	ctx := context.Background()
	db := client.Database(fmt.Sprintf("%s-delete-%d", dbName, time.Now().UnixNano()))
	defer func() { _ = db.Drop(context.Background()) }()
	b, err := OpenBucket(db.GridFSBucket(options.GridFSBucket().SetName(bucketName1)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if err := b.WriteAll(ctx, "key", []byte("data"), nil); err != nil {
		t.Fatalf("writing the blob: %v", err)
	}
	chunks, aside := bucketName1+".chunks", bucketName1+".chunks-aside"
	rename := func(from, to string) {
		t.Helper()
		cmd := bson.D{{Key: "renameCollection", Value: db.Name() + "." + from}, {Key: "to", Value: db.Name() + "." + to}}
		if err := client.Database("admin").RunCommand(ctx, cmd).Err(); err != nil {
			t.Fatalf("renaming %s to %s: %v", from, to, err)
		}
	}
	rename(chunks, aside)
	if err := db.CreateView(ctx, chunks, aside, mongo.Pipeline{}); err != nil {
		t.Fatalf("creating the view: %v", err)
	}

	if err := b.Delete(ctx, "key"); err == nil {
		t.Fatal("got no error deleting the chunks from a view")
	}

	// The chunks can be deleted again: the chunks collection replaces the view.
	if err := db.Collection(chunks).Drop(ctx); err != nil {
		t.Fatalf("dropping the view: %v", err)
	}
	rename(aside, chunks)
	if exists, err := b.Exists(ctx, "key"); err != nil || !exists {
		t.Fatalf("Exists after the failed deletion = %v, %v, want the blob kept for the retry", exists, err)
	}
	if err := b.Delete(ctx, "key"); err != nil {
		t.Fatalf("retrying the deletion: %v", err)
	}
	if exists, err := b.Exists(ctx, "key"); err != nil || exists {
		t.Errorf("Exists after the retry = %v, %v, want the blob deleted", exists, err)
	}
	assertNoOrphanChunks(t, db)
}
