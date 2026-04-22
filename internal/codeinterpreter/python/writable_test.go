package python

import (
	"bytes"
	"context"
	"io/fs"
	"sync"
	"testing"
	"time"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	wsys "github.com/tetratelabs/wazero/sys"
)

// memFS is a minimal writable filesystem used to verify that the Python
// sandbox's file writes reach the host. It satisfies wazeroMountable so
// python.Run routes through WithSysFSMount.
type memFS struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (m *memFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (m *memFS) AsWazeroFS() experimentalsys.FS { return &memSysFS{m: m} }

func (m *memFS) get(name string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[name]
	return b, ok
}

func (m *memFS) put(name string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files == nil {
		m.files = map[string][]byte{}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.files[name] = cp
}

type memSysFS struct {
	experimentalsys.UnimplementedFS
	m *memFS
}

func (a *memSysFS) OpenFile(path string, flag experimentalsys.Oflag, _ fs.FileMode) (experimentalsys.File, experimentalsys.Errno) {
	if path == "" || path == "." {
		return &memDir{}, 0
	}
	writing := flag&experimentalsys.O_WRONLY != 0 || flag&experimentalsys.O_CREAT != 0 || flag&experimentalsys.O_TRUNC != 0
	if !writing {
		if b, ok := a.m.get(path); ok {
			return &memReadFile{data: b}, 0
		}
		return nil, experimentalsys.ENOENT
	}
	return &memWriteFile{m: a.m, path: path, buf: &bytes.Buffer{}}, 0
}

func (a *memSysFS) Stat(path string) (wsys.Stat_t, experimentalsys.Errno) {
	if path == "" || path == "." {
		return wsys.Stat_t{Mode: fs.ModeDir | 0755}, 0
	}
	if b, ok := a.m.get(path); ok {
		return wsys.Stat_t{Mode: 0644, Size: int64(len(b))}, 0
	}
	return wsys.Stat_t{}, experimentalsys.ENOENT
}

func (a *memSysFS) Lstat(path string) (wsys.Stat_t, experimentalsys.Errno) { return a.Stat(path) }

type memDir struct{ experimentalsys.UnimplementedFile }

func (memDir) IsDir() (bool, experimentalsys.Errno) { return true, 0 }
func (memDir) Stat() (wsys.Stat_t, experimentalsys.Errno) {
	return wsys.Stat_t{Mode: fs.ModeDir | 0755}, 0
}
func (memDir) Readdir(int) ([]experimentalsys.Dirent, experimentalsys.Errno) { return nil, 0 }
func (memDir) Close() experimentalsys.Errno                                  { return 0 }

type memReadFile struct {
	experimentalsys.UnimplementedFile
	data []byte
	off  int64
}

func (f *memReadFile) IsDir() (bool, experimentalsys.Errno) { return false, 0 }
func (f *memReadFile) Stat() (wsys.Stat_t, experimentalsys.Errno) {
	return wsys.Stat_t{Mode: 0644, Size: int64(len(f.data))}, 0
}

func (f *memReadFile) Read(buf []byte) (int, experimentalsys.Errno) {
	if f.off >= int64(len(f.data)) {
		return 0, 0
	}
	n := copy(buf, f.data[f.off:])
	f.off += int64(n)
	return n, 0
}

func (f *memReadFile) Close() experimentalsys.Errno { return 0 }

type memWriteFile struct {
	experimentalsys.UnimplementedFile
	m      *memFS
	path   string
	buf    *bytes.Buffer
	closed bool
}

func (f *memWriteFile) IsDir() (bool, experimentalsys.Errno) { return false, 0 }
func (f *memWriteFile) Stat() (wsys.Stat_t, experimentalsys.Errno) {
	return wsys.Stat_t{Mode: 0644, Size: int64(f.buf.Len())}, 0
}

func (f *memWriteFile) Write(p []byte) (int, experimentalsys.Errno) {
	n, _ := f.buf.Write(p) // bytes.Buffer.Write never errors
	return n, 0
}

func (f *memWriteFile) Close() experimentalsys.Errno {
	if f.closed {
		return 0
	}
	f.closed = true
	f.m.put(f.path, f.buf.Bytes())
	return 0
}

func TestRunWritesToRoot(t *testing.T) {
	t.Parallel()
	const payload = "python says hi"

	mfs := &memFS{}

	code := `with open("/out.txt", "w") as f:
    f.write("` + payload + `")`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := Run(ctx, mfs, code)
	if err != nil {
		t.Logf("stdout: %s", res.Stdout)
		t.Logf("stderr: %s", res.Stderr)
		t.Logf("platform error: %s", res.PlatformError)
		t.Fatal(err)
	}

	got, ok := mfs.get("out.txt")
	if !ok {
		t.Logf("stderr: %s", res.Stderr)
		t.Fatal("python did not create /out.txt on the host fs")
	}
	if string(got) != payload {
		t.Fatalf("payload mismatch:\nwant=%q\ngot =%q", payload, got)
	}
}
