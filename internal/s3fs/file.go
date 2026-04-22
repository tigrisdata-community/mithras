package s3fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var (
	_ fs.File     = (*file)(nil)
	_ fs.FileInfo = (*fileInfo)(nil)
	_ io.Seeker   = (*file)(nil)
	_ io.Writer   = (*file)(nil)
)

// errReadOnWrite is returned when Read is called on a file opened for writing.
var errReadOnWrite = errors.New("s3fs: read on write-opened file")

// errWriteOnRead is returned when Write is called on a file opened for reading.
var errWriteOnRead = errors.New("s3fs: write on read-opened file")

type file struct {
	cl     Client
	bucket string
	name   string

	// mode is os.O_RDONLY for read opens, os.O_WRONLY for write opens.
	mode int

	// Read-mode fields.
	io.ReadCloser
	stat   func() (fs.FileInfo, error)
	offset int64
	eTag   string

	// Write-mode fields.
	pw       *io.PipeWriter
	uploadCh chan error // receives the uploader's final error, then closes
	closed   bool
}

func openFile(cl Client, bucket string, name string) (fs.File, error) {
	out, err := cl.GetObject(context.Background(), &s3.GetObjectInput{
		Key:    &name,
		Bucket: &bucket,
	})

	if err != nil {
		return nil, err
	}

	statFunc := getStatFunc(cl, bucket, name, *out)

	return &file{
		cl:         cl,
		bucket:     bucket,
		name:       name,
		mode:       os.O_RDONLY,
		ReadCloser: out.Body,
		stat:       statFunc,
		offset:     0,
		eTag:       *out.ETag,
	}, nil
}

// openFileWrite opens name for streaming write. A goroutine runs a
// manager.Uploader against the read side of an io.Pipe; the returned
// file's Write feeds the write side. Close flushes the pipe and blocks
// on the uploader's final result.
func openFileWrite(cl Client, bucket, name string) (*file, error) {
	pr, pw := io.Pipe()
	uploadCh := make(chan error, 1)

	go func() {
		uploader := manager.NewUploader(cl)
		_, err := uploader.Upload(context.Background(), &s3.PutObjectInput{
			Bucket: &bucket,
			Key:    &name,
			Body:   pr,
		})
		if err != nil {
			// Surface the error to any in-flight Write caller by
			// closing the pipe reader with it.
			pr.CloseWithError(err)
		}
		uploadCh <- err
		close(uploadCh)
	}()

	return &file{
		cl:     cl,
		bucket: bucket,
		name:   name,
		mode:   os.O_WRONLY,
		pw:     pw,
		stat: func() (fs.FileInfo, error) {
			return &fileInfo{name: path.Base(name)}, nil
		},
		uploadCh: uploadCh,
	}, nil
}

func getStatFunc(cl Client, bucket string, name string, s3ObjOutput s3.GetObjectOutput) func() (fs.FileInfo, error) {
	statFunc := func() (fs.FileInfo, error) {
		return stat(cl, bucket, name)
	}

	if s3ObjOutput.ContentLength != nil && s3ObjOutput.LastModified != nil {
		// if we got all the information from GetObjectOutput
		// then we can cache fileinfo instead of making
		// another call in case Stat is called.
		statFunc = func() (fs.FileInfo, error) {
			return &fileInfo{
				name:    path.Base(name),
				size:    *s3ObjOutput.ContentLength,
				modTime: *s3ObjOutput.LastModified,
			}, nil
		}
	}

	return statFunc
}

func (f *file) Read(p []byte) (int, error) {
	if f.mode == os.O_WRONLY {
		return 0, errReadOnWrite
	}
	n, err := f.ReadCloser.Read(p)
	f.offset += int64(n)
	return n, err
}

// Write sends data to the upload pipe. It returns errWriteOnRead if the
// file was opened for reading. If the uploader goroutine has already
// surfaced an error, subsequent writes return that error.
func (f *file) Write(p []byte) (int, error) {
	if f.mode != os.O_WRONLY {
		return 0, errWriteOnRead
	}
	return f.pw.Write(p)
}

// Close finalises the file. For write opens, it closes the pipe (signals
// EOF to the uploader) and blocks until the uploader returns, surfacing
// any upload error. For read opens, it closes the underlying body.
func (f *file) Close() error {
	if f.mode == os.O_WRONLY {
		if f.closed {
			return nil
		}
		f.closed = true
		if err := f.pw.Close(); err != nil {
			// Drain the uploader result so the goroutine exits.
			<-f.uploadCh
			return err
		}
		return <-f.uploadCh
	}
	if f.ReadCloser == nil {
		return nil
	}
	return f.ReadCloser.Close()
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	if f.mode == os.O_WRONLY {
		return 0, errors.New("s3fs.file.Seek: not supported on write-opened files")
	}
	newOffset := f.offset

	stat, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := stat.Size()

	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset += offset
	case io.SeekEnd:
		newOffset = size + offset
	default:
		return 0, errors.New("s3fs.file.Seek: invalid whence")
	}

	// If the position has not moved, there is no need to make a new query
	if f.offset == newOffset {
		return newOffset, nil
	}

	if newOffset < 0 {
		return 0, errors.New("s3fs.file.Seek: seeked to a negative position")
	}

	if f.eTag == "" {
		return 0, errors.New("s3fs.file.Seek: cannot seek. remote file has no etag")
	}

	if err := f.Close(); err != nil {
		return f.offset, err
	}

	if newOffset >= size {
		f.ReadCloser = io.NopCloser(eofReader{})
		f.offset = newOffset
		return f.offset, nil
	}

	rawObject, err := f.cl.GetObject(
		context.Background(),
		&s3.GetObjectInput{
			Bucket:  &f.bucket,
			Key:     &f.name,
			Range:   ptr(fmt.Sprintf("bytes=%d-", newOffset)),
			IfMatch: &f.eTag,
		})

	if err != nil {
		if e := new(awshttp.ResponseError); errors.As(err, &e) {
			if e.HTTPStatusCode() == http.StatusPreconditionFailed {
				return 0, fmt.Errorf("s3fs.file.Seek: file has changed while seeking: %w", fs.ErrNotExist)
			}
		}
		return 0, err
	}

	f.offset = newOffset
	f.ReadCloser = rawObject.Body

	return f.offset, nil
}

func (f file) Stat() (fs.FileInfo, error) { return f.stat() }

type fileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (fi fileInfo) Name() string       { return path.Base(fi.name) }
func (fi fileInfo) Size() int64        { return fi.size }
func (fi fileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi fileInfo) ModTime() time.Time { return fi.modTime }
func (fi fileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi fileInfo) Sys() interface{}   { return nil }

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

func ptr[T any](v T) *T {
	return &v
}
