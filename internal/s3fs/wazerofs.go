package s3fs

import (
	"errors"
	"io"
	"io/fs"

	sysexp "github.com/tetratelabs/wazero/experimental/sys"
	wsys "github.com/tetratelabs/wazero/sys"
)

// WazeroFS adapts *S3FS to wazero's experimental/sys.FS so it can be
// mounted as a writeable filesystem inside a wazero WASI sandbox.
//
// Most syscall-adjacent methods are inherited from sys.UnimplementedFS.
// Only the operations that S3 can service are overridden:
//
//   - OpenFile: routes to openFile (read) or openFileWrite (write).
//   - Stat/Lstat: forwards to S3FS.Stat.
//   - Mkdir: forwards to S3FS.MkdirAll (single-segment mkdir is just
//     MkdirAll of one marker).
//   - Unlink: forwards to S3FS.Remove.
type WazeroFS struct {
	sysexp.UnimplementedFS
	fs *S3FS
}

// AsWazeroFS returns a wazero sys.FS view over f. Pass the result to
// experimental/sysfs.FSConfig.WithSysFSMount to give the WASI guest
// writable access to the bucket.
func (f *S3FS) AsWazeroFS() sysexp.FS { return &WazeroFS{fs: f} }

// OpenFile implements sys.FS.
func (a *WazeroFS) OpenFile(path string, flag sysexp.Oflag, perm fs.FileMode) (sysexp.File, sysexp.Errno) {
	writing := flag&sysexp.O_WRONLY != 0 || flag&sysexp.O_CREAT != 0 || flag&sysexp.O_TRUNC != 0
	rdwr := flag&sysexp.O_RDWR != 0

	// S3 objects are not simultaneously readable and writable through
	// the same handle.
	if rdwr {
		return nil, sysexp.EINVAL
	}

	if writing {
		wf, err := a.fs.Create(path)
		if err != nil {
			return nil, toErrno(err)
		}
		return &wazeroWriteFile{wc: wf, name: path}, 0
	}

	f, err := a.fs.Open(path)
	if err != nil {
		return nil, toErrno(err)
	}
	return &wazeroReadFile{f: f}, 0
}

// Stat implements sys.FS.
func (a *WazeroFS) Stat(path string) (wsys.Stat_t, sysexp.Errno) {
	fi, err := a.fs.Stat(path)
	if err != nil {
		return wsys.Stat_t{}, toErrno(err)
	}
	return wsys.NewStat_t(fi), 0
}

// Lstat implements sys.FS. S3 has no symlinks, so Lstat is identical to Stat.
func (a *WazeroFS) Lstat(path string) (wsys.Stat_t, sysexp.Errno) {
	return a.Stat(path)
}

// Mkdir implements sys.FS by writing a single directory marker.
// MkdirAll-style behavior is safe here because S3 prefixes are implicit.
func (a *WazeroFS) Mkdir(path string, perm fs.FileMode) sysexp.Errno {
	if err := a.fs.MkdirAll(path, perm); err != nil {
		return toErrno(err)
	}
	return 0
}

// Unlink implements sys.FS by deleting the object at path.
func (a *WazeroFS) Unlink(path string) sysexp.Errno {
	if err := a.fs.Remove(path); err != nil {
		return toErrno(err)
	}
	return 0
}

// Rmdir removes the directory marker at path. Object deletion is
// idempotent in S3, so the behavior is the same as Unlink for us.
func (a *WazeroFS) Rmdir(path string) sysexp.Errno {
	marker := path + "/"
	if err := a.fs.Remove(marker); err != nil {
		return toErrno(err)
	}
	return 0
}

// wazeroReadFile wraps a read-opened fs.File as a sys.File.
type wazeroReadFile struct {
	sysexp.UnimplementedFile
	f fs.File
}

func (w *wazeroReadFile) IsDir() (bool, sysexp.Errno) {
	fi, err := w.f.Stat()
	if err != nil {
		return false, toErrno(err)
	}
	return fi.IsDir(), 0
}

func (w *wazeroReadFile) Stat() (wsys.Stat_t, sysexp.Errno) {
	fi, err := w.f.Stat()
	if err != nil {
		return wsys.Stat_t{}, toErrno(err)
	}
	return wsys.NewStat_t(fi), 0
}

func (w *wazeroReadFile) Read(buf []byte) (int, sysexp.Errno) {
	n, err := w.f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, toErrno(err)
	}
	return n, 0
}

// Seek implements sys.File. The signature intentionally returns sys.Errno
// to satisfy wazero's experimental/sys.File contract, which conflicts with
// the io.Seeker shape that go vet's stdmethods analyzer expects.
//
//nolint:stdmethods
func (w *wazeroReadFile) Seek(offset int64, whence int) (int64, sysexp.Errno) {
	s, ok := w.f.(io.Seeker)
	if !ok {
		return 0, sysexp.ENOSYS
	}
	n, err := s.Seek(offset, whence)
	if err != nil {
		return n, toErrno(err)
	}
	return n, 0
}

func (w *wazeroReadFile) Readdir(n int) ([]sysexp.Dirent, sysexp.Errno) {
	d, ok := w.f.(fs.ReadDirFile)
	if !ok {
		return nil, sysexp.ENOTDIR
	}
	entries, err := d.ReadDir(n)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, toErrno(err)
	}
	out := make([]sysexp.Dirent, 0, len(entries))
	for _, e := range entries {
		typ := fs.FileMode(0)
		if e.IsDir() {
			typ = fs.ModeDir
		}
		out = append(out, sysexp.Dirent{Name: e.Name(), Type: typ})
	}
	return out, 0
}

func (w *wazeroReadFile) Close() sysexp.Errno {
	if err := w.f.Close(); err != nil {
		return toErrno(err)
	}
	return 0
}

// wazeroWriteFile wraps a write-opened io.WriteCloser as a sys.File.
type wazeroWriteFile struct {
	sysexp.UnimplementedFile
	wc   io.WriteCloser
	name string
}

func (w *wazeroWriteFile) IsDir() (bool, sysexp.Errno) { return false, 0 }

func (w *wazeroWriteFile) Stat() (wsys.Stat_t, sysexp.Errno) {
	return wsys.Stat_t{Mode: 0644}, 0
}

func (w *wazeroWriteFile) Write(buf []byte) (int, sysexp.Errno) {
	n, err := w.wc.Write(buf)
	if err != nil {
		return n, toErrno(err)
	}
	return n, 0
}

func (w *wazeroWriteFile) Close() sysexp.Errno {
	if err := w.wc.Close(); err != nil {
		return toErrno(err)
	}
	return 0
}

// toErrno maps a Go error returned by S3FS to the closest sys.Errno.
// Unknown errors collapse to EIO, which is how the rest of the wazero
// ecosystem handles unexpected syscall failures.
func toErrno(err error) sysexp.Errno {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return sysexp.ENOENT
	case errors.Is(err, fs.ErrInvalid):
		return sysexp.EINVAL
	case errors.Is(err, fs.ErrPermission):
		return sysexp.EACCES
	case errors.Is(err, fs.ErrExist):
		return sysexp.EEXIST
	}
	return sysexp.EIO
}
