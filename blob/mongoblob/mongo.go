package mongoblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/escape"
	"gocloud.dev/internal/gcerr"
)

const defaultPageSize = 1000

var (
	errNotFound       = errors.New("blob not found")
	errNotImplemented = errors.New("not implemented")
)

// Dial returns a new mongoDB client that is connected to the server URI.
func Dial(uri string) (*mongo.Client, error) {
	opts := options.Client().ApplyURI(uri)
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	return client, nil
}

type bucket struct {
	bucket *mongo.GridFSBucket
}

func newBucket(b *mongo.GridFSBucket) *bucket {
	return &bucket{bucket: b}
}

// OpenBucket opens a MongoDB bucket for use with Blob.
func OpenBucket(mongoBucket *mongo.GridFSBucket) (*blob.Bucket, error) {
	b := newBucket(mongoBucket)
	return blob.NewBucket(b), nil
}

func (b *bucket) ErrorCode(err error) gcerrors.ErrorCode {
	var g *gcerr.Error
	if errors.As(err, &g) {
		return g.Code
	}
	if errors.Is(err, errNotFound) {
		return gcerrors.NotFound
	}
	if errors.Is(err, errNotImplemented) {
		return gcerrors.Unimplemented
	}
	return gcerrors.Unknown
}

// As implements driver.As.
func (b *bucket) As(i any) bool {
	p, ok := i.(**mongo.GridFSBucket)
	if !ok {
		return false
	}
	*p = b.bucket
	return true
}

// ErrorAs implements driver.Bucket.ErrorAs
func (b *bucket) ErrorAs(err error, a any) bool {
	switch e := err.(type) {
	case mongo.CommandError:
		if p, ok := a.(*mongo.CommandError); ok {
			*p = e
			return true
		}
	case mongo.BulkWriteError:
		if p, ok := a.(*mongo.BulkWriteError); ok {
			*p = e
			return true
		}
	case mongo.BulkWriteException:
		if p, ok := a.(*mongo.BulkWriteException); ok {
			*p = e
			return true
		}
	}
	return false
}

func (b *bucket) Attributes(ctx context.Context, key string) (*driver.Attributes, error) {
	docs, err := b.getDocuments(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, errNotFound
	}
	return docs[0].driverAttributes(), nil
}

func (b *bucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	var filter bson.D
	if opts.Prefix != "" {
		filter = bson.D{{"filename", bson.D{{"$regex", "^" + strings.ReplaceAll(opts.Prefix, "\\", "\\\\")}}}}
	}
	pageSize := int32(opts.PageSize)
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	// We take an additional element to check if it's the last page or not
	pageSize++

	if len(opts.PageToken) > 0 {
		filter = append(filter, bson.E{Key: "filename", Value: bson.D{{"$gt", string(opts.PageToken)}}})
	}
	findOptions := options.GridFSFind().SetSort(bson.D{{"filename", 1}, {"uploadDate", -1}}).SetLimit(pageSize)
	cursor, err := b.bucket.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, err
	}
	var gridFsFiles []gridFSFile
	if err := cursor.All(ctx, &gridFsFiles); err != nil {
		return nil, err
	}

	if len(gridFsFiles) == 0 {
		return &driver.ListPage{Objects: nil}, nil
	}
	// We check if it's the last page
	isLastPage := len(gridFsFiles) < int(pageSize)

	if !isLastPage {
		// Only for the non latest page we resize to the actual page size
		gridFsFiles = gridFsFiles[:len(gridFsFiles)-1]
		// For the latest objects we need to ensure we have all the versions of a given objects in the page.
		docs, err := b.getDocuments(ctx, gridFsFiles[len(gridFsFiles)-1].Name)
		if err != nil {
			return nil, err
		}
		nonExistingDocs := lo.Filter(docs, func(f1 gridFSFile, _ int) bool {
			return !lo.ContainsBy(gridFsFiles, func(f2 gridFSFile) bool { return f2.ID == f1.ID })
		})
		gridFsFiles = append(gridFsFiles, nonExistingDocs...)
	}

	// If opts.Delimiter != "", lastPrefix contains the last "directory" key we
	// added. It is used to avoid adding it again; all files in this "directory"
	// are collapsed to the single directory entry.
	var lastPrefix string
	if opts.Delimiter != "" && len(opts.PageToken) > 0 {
		keyWithoutPrefix := string(opts.PageToken)[len(opts.Prefix):]
		if idx := strings.Index(keyWithoutPrefix, opts.Delimiter); idx != -1 {
			lastPrefix = opts.Prefix + keyWithoutPrefix[0:idx+len(opts.Delimiter)]
		}
	}
	objs := make([]*driver.ListObject, 0, len(gridFsFiles))
	seen := make(map[string]bool)
	for _, file := range gridFsFiles {
		if seen[file.Name] {
			continue
		}
		obj := &driver.ListObject{
			Key:     file.Name,
			ModTime: file.Metadata.ModTime,
			Size:    file.Length,
		}
		// If using Delimiter, collapse "directories".
		if opts.Delimiter != "" {
			// Strip the prefix, which may contain Delimiter.
			keyWithoutPrefix := file.Name[len(opts.Prefix):]
			// See if the key still contains Delimiter.
			// If no, it's a file, and we just include it.
			// If yes, it's a file in a "subdirectory" and we want to collapse
			// all files in that "subdirectory" into a single "directory" result.
			if idx := strings.Index(keyWithoutPrefix, opts.Delimiter); idx != -1 {
				prefix := opts.Prefix + keyWithoutPrefix[0:idx+len(opts.Delimiter)]
				// We've already included this "directory"; don't add it.
				if prefix == lastPrefix {
					continue
				}
				// Update the object to be a "directory".
				obj = &driver.ListObject{
					Key:   prefix,
					IsDir: true,
				}
				lastPrefix = prefix
			}
		}
		objs = append(objs, obj)
		seen[file.Name] = true
	}

	var nextPageToken []byte
	if !isLastPage {
		nextPageToken = []byte(gridFsFiles[len(gridFsFiles)-1].Name)
	}
	return &driver.ListPage{
		Objects:       objs,
		NextPageToken: nextPageToken,
	}, nil
}

func (b *bucket) NewRangeReader(ctx context.Context, key string, offset, length int64, _ *driver.ReaderOptions) (driver.Reader, error) {
	docs, err := b.getDocuments(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, errNotFound
	}
	d, err := b.bucket.OpenDownloadStream(ctx, docs[0].ID)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := d.Skip(offset); err != nil {
			return nil, err
		}
	}
	var ior io.Reader = d
	if length >= 0 {
		ior = io.LimitReader(d, length)
	}
	return &reader{
		r:   ior,
		raw: d,
		attrs: driver.ReaderAttributes{
			ContentType: docs[0].Metadata.ContentType,
			ModTime:     docs[0].Metadata.ModTime,
			Size:        docs[0].Length,
		},
	}, nil
}

type reader struct {
	r     io.Reader
	raw   *mongo.GridFSDownloadStream
	attrs driver.ReaderAttributes
}

func (r *reader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *reader) Close() error {
	return r.raw.Close()
}

func (r *reader) Attributes() *driver.ReaderAttributes {
	return &r.attrs
}

func (r *reader) As(any) bool { return false }

func (b *bucket) NewTypedWriter(ctx context.Context, key, contentType string, opts *driver.WriterOptions) (driver.Writer, error) {
	if key == "" {
		return nil, errors.New("invalid key (empty string)")
	}
	docs, err := b.getDocuments(ctx, key)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	createTime := now
	if len(docs) > 0 {
		createTime = docs[0].Metadata.CreateTime
	}
	md := make(map[string]string, len(opts.Metadata))
	for k, v := range opts.Metadata {
		k = escape.HexEscape(k, func(runes []rune, i int) bool {
			c := runes[i]
			return c <= 16
		})
		md[k] = v
	}
	attrs := attributes{
		CacheControl:       opts.CacheControl,
		ContentDisposition: opts.ContentDisposition,
		ContentEncoding:    opts.ContentEncoding,
		ContentLanguage:    opts.ContentLanguage,
		ContentType:        contentType,
		Metadata:           md,
		CreateTime:         createTime,
		ModTime:            now,
		ETag:               fmt.Sprintf("\"%d\"", now.UnixNano()),
	}
	stream, err := b.bucket.OpenUploadStream(ctx, key, options.GridFSUpload().SetMetadata(attrs))
	if err != nil {
		return nil, err
	}
	return &writer{
		b:            b,
		w:            stream,
		existingDocs: docs,
		ctx:          ctx,
		ifNotExist:   opts.IfNotExist,
		key:          key,
	}, nil
}

type writer struct {
	w            *mongo.GridFSUploadStream
	existingDocs []gridFSFile
	b            *bucket
	ctx          context.Context
	ifNotExist   bool
	key          string
}

func (w *writer) Write(p []byte) (n int, err error) {
	// The error is expected to be returned when writing
	if w.ifNotExist && len(w.existingDocs) > 0 {
		err := fmt.Errorf("a blob already exists for key %q", w.key)
		return 0, gcerr.New(gcerrors.FailedPrecondition, err, 1, "IfNotExist precondition failed")
	}
	return w.w.Write(p)
}

func (w *writer) Close() error {
	if err := w.w.Close(); err != nil {
		return err
	}
	for _, doc := range w.existingDocs {
		if err := w.b.bucket.Delete(w.ctx, doc.ID); err != nil {
			// We ignore the error as the file has been correctly written
			log.Printf("failed to delete old blob with id %v: %v", doc.ID, err)
		}
	}
	return nil
}

func (b *bucket) Copy(ctx context.Context, dstKey, srcKey string, _ *driver.CopyOptions) error {
	docs, err := b.getDocuments(ctx, srcKey)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return errNotFound
	}
	d, err := b.bucket.OpenDownloadStream(ctx, docs[0].ID)
	if err != nil {
		return err
	}
	attrs := docs[0].Metadata
	attrs.CreateTime = time.Now()
	attrs.ModTime = time.Now()
	if _, err := b.bucket.UploadFromStream(ctx, dstKey, d, options.GridFSUpload().SetMetadata(attrs)); err != nil {
		return err
	}
	return nil
}

func (b *bucket) Delete(ctx context.Context, key string, opts *driver.DeleteOptions) error {
	if opts.BeforeDelete != nil {
		// mongoblob exposes no driver-specific delete type; the hook is still
		// invoked exactly once so portable callers behave consistently.
		if err := opts.BeforeDelete(func(any) bool { return false }); err != nil {
			return err
		}
	}
	docs, err := b.getDocuments(ctx, key)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return errNotFound
	}
	for _, doc := range docs {
		if err := b.bucket.Delete(ctx, doc.ID); err != nil {
			return err
		}
	}
	return nil
}

// getDocuments returns all the mongo documents for the specified keys order by upload date descending
func (b *bucket) getDocuments(ctx context.Context, key string) ([]gridFSFile, error) {
	cursor, err := b.bucket.Find(ctx, bson.D{{"filename", key}})
	if err != nil {
		return nil, err
	}
	var files []gridFSFile
	if err := cursor.All(ctx, &files); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].UploadDate.After(files[j].UploadDate) })
	return files, nil
}

func (b *bucket) SignedURL(ctx context.Context, key string, opts *driver.SignedURLOptions) (string, error) {
	return "", errNotImplemented
}

// Close implements driver.Bucket.Close.
func (b *bucket) Close() error {
	return nil
}

type gridFSFile struct {
	ID         bson.ObjectID `bson:"_id"`
	Name       string        `bson:"filename"`
	Length     int64         `bson:"length"`
	UploadDate time.Time     `bson:"uploadDate"`
	Metadata   attributes    `bson:"metadata"`
}

func (f gridFSFile) driverAttributes() *driver.Attributes {
	md := make(map[string]string, len(f.Metadata.Metadata))
	for k, v := range f.Metadata.Metadata {
		md[escape.HexUnescape(k)] = v
	}
	return &driver.Attributes{
		CacheControl:       f.Metadata.CacheControl,
		ContentDisposition: f.Metadata.ContentDisposition,
		ContentEncoding:    f.Metadata.ContentEncoding,
		ContentLanguage:    f.Metadata.ContentLanguage,
		ContentType:        f.Metadata.ContentType,
		Metadata:           md,
		CreateTime:         f.Metadata.CreateTime,
		ModTime:            f.Metadata.ModTime,
		Size:               f.Length,
		ETag:               f.Metadata.ETag,
	}
}

type attributes struct {
	CacheControl       string            `bson:"cacheControl"`
	ContentDisposition string            `bson:"contentDisposition"`
	ContentEncoding    string            `bson:"contentEncoding"`
	ContentLanguage    string            `bson:"contentLanguage"`
	ContentType        string            `bson:"contentType"`
	Metadata           map[string]string `bson:"metadata"`
	CreateTime         time.Time         `bson:"createTime"`
	ModTime            time.Time         `bson:"modTime"`
	ETag               string            `bson:"eTag"`
}
