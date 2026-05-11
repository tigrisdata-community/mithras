package s3fs

import (
	"io"
	stdfs "io/fs"
	"os"
	"path"
	"strings"

	"github.com/go-git/go-billy/v5"
)

var _ billy.Filesystem = (*billyAdapter)(nil)

// AsBilly returns a billy.Filesystem view of f. Backed by the same S3 bucket
// the underlying *S3FS targets, so writes through the billy interface land
// directly in S3.
func (f *S3FS) AsBilly() billy.Filesystem {
	return &billyAdapter{fs: f}
}

// billyAdapter exposes *S3FS as a billy.Basic + billy.Dir + billy.Chroot. S3
// has no symlinks, no chmod, and no real chroot, so those interfaces are
// implemented as no-ops or ErrNotSupported.
type billyAdapter struct {
	fs *S3FS
}

// cleanPath translates a billy-style path (which may carry a leading slash and
// may be ".") into a path acceptable to fs.ValidPath / S3FS.
func (a *billyAdapter) cleanPath(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	p = path.Clean(p)
	if p == "" || p == "." {
		return ".", nil
	}
	if !stdfs.ValidPath(p) {
		return "", &stdfs.PathError{Op: "open", Path: p, Err: stdfs.ErrInvalid}
	}
	return p, nil
}

func (a *billyAdapter) Create(filename string) (billy.File, error) {
	return a.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
}

func (a *billyAdapter) Open(filename string) (billy.File, error) {
	return a.OpenFile(filename, os.O_RDONLY, 0)
}

func (a *billyAdapter) OpenFile(filename string, flag int, _ os.FileMode) (billy.File, error) {
	name, err := a.cleanPath(filename)
	if err != nil {
		return nil, err
	}

	// S3 has no concurrent read+write handle. Treat any write-bit as
	// open-for-write and ignore O_APPEND/O_EXCL semantics that S3 cannot
	// honor in a single PUT.
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		wc, err := a.fs.Create(name)
		if err != nil {
			return nil, err
		}
		return &billyFile{name: name, wc: wc}, nil
	}

	rf, err := a.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return &billyFile{name: name, rf: rf}, nil
}

func (a *billyAdapter) Stat(filename string) (os.FileInfo, error) {
	name, err := a.cleanPath(filename)
	if err != nil {
		return nil, err
	}
	return a.fs.Stat(name)
}

// Rename is unsupported: S3 has no rename primitive, only copy+delete.
func (a *billyAdapter) Rename(_, _ string) error { return billy.ErrNotSupported }

func (a *billyAdapter) Remove(filename string) error {
	name, err := a.cleanPath(filename)
	if err != nil {
		return err
	}
	return a.fs.Remove(name)
}

// Join uses forward slashes regardless of host OS — billy paths inside a WASI
// guest are POSIX.
func (a *billyAdapter) Join(elem ...string) string {
	return path.Join(elem...)
}

func (a *billyAdapter) ReadDir(p string) ([]os.FileInfo, error) {
	name, err := a.cleanPath(p)
	if err != nil {
		return nil, err
	}
	entries, err := a.fs.ReadDir(name)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}

func (a *billyAdapter) MkdirAll(p string, perm os.FileMode) error {
	name, err := a.cleanPath(p)
	if err != nil {
		return err
	}
	if name == "." {
		return nil
	}
	return a.fs.MkdirAll(name, perm)
}

// Chroot is a no-op for S3 — there is no real root to confine. Returning the
// same adapter keeps the billy.Filesystem contract satisfied without
// promising boundary enforcement.
func (a *billyAdapter) Chroot(_ string) (billy.Filesystem, error) { return a, nil }

func (a *billyAdapter) Root() string { return "/" }

// Symlink interface stubs — S3 does not model symlinks.
func (a *billyAdapter) Lstat(filename string) (os.FileInfo, error)   { return a.Stat(filename) }
func (a *billyAdapter) Symlink(_, _ string) error                    { return billy.ErrNotSupported }
func (a *billyAdapter) Readlink(_ string) (string, error)            { return "", billy.ErrNotSupported }
func (a *billyAdapter) TempFile(_, _ string) (billy.File, error)     { return nil, billy.ErrNotSupported }

// billyFile carries either a read-opened fs.File or a write-opened
// io.WriteCloser; never both.
type billyFile struct {
	name string
	rf   stdfs.File
	wc   io.WriteCloser
}

func (b *billyFile) Name() string { return b.name }

func (b *billyFile) Read(p []byte) (int, error) {
	if b.rf == nil {
		return 0, errReadOnWrite
	}
	return b.rf.Read(p)
}

func (b *billyFile) Write(p []byte) (int, error) {
	if b.wc == nil {
		return 0, errWriteOnRead
	}
	return b.wc.Write(p)
}

// ReadAt synthesises pread by seeking the read handle. Returns
// billy.ErrNotSupported if the file isn't seekable (the default
// non-WithReadSeeker mode).
func (b *billyFile) ReadAt(p []byte, off int64) (int, error) {
	if b.rf == nil {
		return 0, errReadOnWrite
	}
	s, ok := b.rf.(io.Seeker)
	if !ok {
		return 0, billy.ErrNotSupported
	}
	if _, err := s.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(b.rf, p)
}

func (b *billyFile) Seek(off int64, whence int) (int64, error) {
	if b.rf == nil {
		return 0, billy.ErrNotSupported
	}
	s, ok := b.rf.(io.Seeker)
	if !ok {
		return 0, billy.ErrNotSupported
	}
	return s.Seek(off, whence)
}

func (b *billyFile) Close() error {
	switch {
	case b.rf != nil:
		return b.rf.Close()
	case b.wc != nil:
		return b.wc.Close()
	}
	return nil
}

func (b *billyFile) Lock() error            { return nil }
func (b *billyFile) Unlock() error          { return nil }
func (b *billyFile) Truncate(_ int64) error { return billy.ErrNotSupported }
