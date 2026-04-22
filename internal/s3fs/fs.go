// Package s3fs provides a S3 implementation for Go1.16 filesystem interface.
package s3fs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var (
	_ fs.FS        = (*S3FS)(nil)
	_ fs.StatFS    = (*S3FS)(nil)
	_ fs.ReadDirFS = (*S3FS)(nil)
	_ CreateFS     = (*S3FS)(nil)
	_ WriteFileFS  = (*S3FS)(nil)
	_ RemoveFS     = (*S3FS)(nil)
	_ MkdirAllFS   = (*S3FS)(nil)
)

// CreateFS is the interface implemented by filesystems that can create
// new files for writing.
type CreateFS interface {
	fs.FS
	Create(name string) (io.WriteCloser, error)
}

// WriteFileFS is the interface implemented by filesystems that can write
// an entire file in a single call.
type WriteFileFS interface {
	fs.FS
	WriteFile(name string, data []byte, perm fs.FileMode) error
}

// RemoveFS is the interface implemented by filesystems that can remove
// a named file.
type RemoveFS interface {
	fs.FS
	Remove(name string) error
}

// MkdirAllFS is the interface implemented by filesystems that can create
// a directory hierarchy.
type MkdirAllFS interface {
	fs.FS
	MkdirAll(path string, perm fs.FileMode) error
}

var errNotDir = errors.New("not a dir")

// Option is a function that provides optional features to S3FS.
type Option func(*S3FS)

// WithReadSeeker enables Seek functionality on files opened with this fs.
//
// BUG(WilliamFrei): Seeking on S3 requires reopening the file at the specified
// position. This can cause problems if the file changed between opening
// and calling Seek. In that case, fs.ErrNotExist error is returned, which
// has to be handled by the caller.
func WithReadSeeker(fsys *S3FS) { fsys.readSeeker = true }

// Client wraps the s3 client methods that this package is using.
// This interface may change in the future and should not be relied on by
// packages using it.
//
// The multipart methods (UploadPart, CreateMultipartUpload,
// CompleteMultipartUpload, AbortMultipartUpload) are the same shape as
// manager.UploadAPIClient so a Client can be passed directly to
// manager.NewUploader for streaming writes.
type Client interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjects(ctx context.Context, params *s3.ListObjectsInput, optFns ...func(*s3.Options)) (*s3.ListObjectsOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)

	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	UploadPart(ctx context.Context, params *s3.UploadPartInput, optFns ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CreateMultipartUpload(ctx context.Context, params *s3.CreateMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	CompleteMultipartUpload(ctx context.Context, params *s3.CompleteMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, params *s3.AbortMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// S3FS is a S3 filesystem implementation.
//
// S3 has a flat structure instead of a hierarchy. S3FS simulates directories
// by using prefixes and delims ("/"). Because directories are simulated, ModTime
// is always a default Time value (IsZero returns true).
type S3FS struct {
	cl         Client
	bucket     string
	readSeeker bool
}

// New returns a new filesystem that works on the specified bucket.
func New(cl Client, bucket string, opts ...Option) *S3FS {
	fsys := &S3FS{
		cl:     cl,
		bucket: bucket,
	}

	for _, opt := range opts {
		opt(fsys)
	}

	return fsys
}

// Open implements fs.FS.
func (f *S3FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  fs.ErrInvalid,
		}
	}

	if name == "." {
		return openDir(f.cl, f.bucket, name)
	}

	file, err := openFile(f.cl, f.bucket, name)

	if err != nil {
		if isNotFoundErr(err) {
			switch d, err := openDir(f.cl, f.bucket, name); {
			case err == nil:
				return d, nil
			case !isNotFoundErr(err) && !errors.Is(err, errNotDir) && !errors.Is(err, fs.ErrNotExist):
				return nil, err
			}

			return nil, &fs.PathError{
				Op:   "open",
				Path: name,
				Err:  fs.ErrNotExist,
			}
		}

		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  err,
		}
	}

	if !f.readSeeker {
		file = fileNoSeek{file}
	}

	return file, nil
}

// Stat implements fs.StatFS.
func (f *S3FS) Stat(name string) (fs.FileInfo, error) {
	fi, err := stat(f.cl, f.bucket, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  err,
		}
	}
	return fi, nil
}

// ReadDir implements fs.ReadDirFS.
func (f *S3FS) ReadDir(name string) ([]fs.DirEntry, error) {
	d, err := openDir(f.cl, f.bucket, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "readdir",
			Path: name,
			Err:  err,
		}
	}
	return d.ReadDir(-1)
}

// Create opens name for writing, returning a streaming io.WriteCloser
// backed by an S3 multipart upload. Bytes written are buffered through
// an io.Pipe into the uploader goroutine and flushed to S3 on Close.
// The object is not visible in the bucket until Close returns with a
// nil error.
func (f *S3FS) Create(name string) (io.WriteCloser, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "create", Path: name, Err: fs.ErrInvalid}
	}
	file, err := openFileWrite(f.cl, f.bucket, name)
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: name, Err: err}
	}
	return file, nil
}

// WriteFile writes data to the named object in one call, using PutObject.
// It bypasses the multipart streaming path, which is more efficient for
// small, already-materialised payloads. perm is accepted for interface
// compatibility but ignored (S3 has no POSIX permissions).
func (f *S3FS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "writefile", Path: name, Err: fs.ErrInvalid}
	}
	_, err := f.cl.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: &f.bucket,
		Key:    &name,
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return &fs.PathError{Op: "writefile", Path: name, Err: err}
	}
	return nil
}

// Remove deletes the object at name from the bucket. Removing a key that
// does not exist is not an error in S3 and is treated the same here.
func (f *S3FS) Remove(name string) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	_, err := f.cl.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: &f.bucket,
		Key:    &name,
	})
	if err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	return nil
}

// MkdirAll creates the directory hierarchy implied by path by writing
// zero-byte marker objects at each ancestor prefix ("foo/", "foo/bar/",
// ...). S3 has no real directories, but these markers let empty
// directories appear in ReadDir listings and satisfy POSIX-minded callers
// (e.g. Python code in the WASI sandbox calling os.makedirs before
// writing a file). perm is accepted for interface compatibility but
// ignored.
func (f *S3FS) MkdirAll(path string, perm fs.FileMode) error {
	if path == "" || path == "." {
		return nil
	}
	if !fs.ValidPath(path) {
		return &fs.PathError{Op: "mkdir", Path: path, Err: fs.ErrInvalid}
	}
	path = strings.TrimSuffix(path, "/")
	parts := strings.Split(path, "/")
	var prefix string
	for _, p := range parts {
		if p == "" {
			continue
		}
		prefix += p + "/"
		key := prefix
		if _, err := f.cl.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: &f.bucket,
			Key:    &key,
			Body:   bytes.NewReader(nil),
		}); err != nil {
			return &fs.PathError{Op: "mkdir", Path: path, Err: err}
		}
	}
	return nil
}

func stat(s3cl Client, bucket, name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}

	if name == "." {
		return &dir{
			s3cl:   s3cl,
			bucket: bucket,
			fileInfo: fileInfo{
				name: ".",
				mode: fs.ModeDir,
			},
		}, nil
	}

	head, err := s3cl.HeadObject(
		context.Background(),
		&s3.HeadObjectInput{
			Bucket: &bucket,
			Key:    &name,
		})
	if err != nil {
		if !isNotFoundErr(err) {
			return nil, err
		}
	} else {
		return &fileInfo{
			name:    name,
			size:    derefInt64(head.ContentLength),
			mode:    0,
			modTime: derefTime(head.LastModified),
		}, nil
	}

	out, err := s3cl.ListObjects(
		context.Background(),
		&s3.ListObjectsInput{
			Bucket:    &bucket,
			Delimiter: ptr("/"),
			Prefix:    ptr(name + "/"),
			MaxKeys:   ptr[int32](1),
		})
	if err != nil {
		return nil, err
	}
	if len(out.CommonPrefixes) > 0 || len(out.Contents) > 0 {
		return &dir{
			s3cl:   s3cl,
			bucket: bucket,
			fileInfo: fileInfo{
				name: name,
				mode: fs.ModeDir,
			},
		}, nil
	}
	return nil, fs.ErrNotExist
}

func openDir(s3cl Client, bucket, name string) (fs.ReadDirFile, error) {
	fi, err := stat(s3cl, bucket, name)
	if err != nil {
		return nil, err
	}

	if d, ok := fi.(fs.ReadDirFile); ok {
		return d, nil
	}
	return nil, errNotDir
}

func isNotFoundErr(err error) bool {
	if e := new(types.NoSuchKey); errors.As(err, &e) {
		return true
	}

	if e := new(http.ResponseError); errors.As(err, &e) {
		// localstack workaround
		if e.HTTPStatusCode() == 404 {
			return true
		}
	}

	return false
}

type fileNoSeek struct{ fs.File }
