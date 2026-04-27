package python

import (
	"bytes"
	"context"
	_ "embed"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tetratelabs/wazero"
	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// wazeroMountable is satisfied by filesystems (like s3fs.S3FS) that can
// expose themselves as a writeable wazero experimental/sys.FS. When
// present we prefer the sys.FS mount so the guest can write back.
type wazeroMountable interface {
	AsWazeroFS() experimentalsys.FS
}

var (
	//go:embed python.wasm
	Binary []byte

	r    wazero.Runtime
	code wazero.CompiledModule
)

func init() {
	ctx := context.Background()
	r = wazero.NewRuntime(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	var err error
	code, err = r.CompileModule(ctx, Binary)
	if err != nil {
		panic(err)
	}
}

type Result struct {
	Stdout        string
	Stderr        string
	PlatformError string
}

// mainPyPath is the path where the generated main.py is placed in the
// guest filesystem. The caller's fs.FS is mounted at /, and a host temp
// directory holding this file is overlaid at /.mithras.
const mainPyPath = "/.mithras/main.py"

func Run(ctx context.Context, fsys fs.FS, userCode string) (*Result, error) {
	fout := &bytes.Buffer{}
	ferr := &bytes.Buffer{}
	fin := &bytes.Buffer{}

	// If fsys is nil, use an empty filesystem.
	if fsys == nil {
		fsys = emptyFS{}
	}

	// Stage main.py in a host temp directory that will be mounted at
	// /.mithras inside the guest, so it sits outside any paths the caller
	// might populate through fsys.
	tmpDir, err := os.MkdirTemp("", "python-wasm-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := os.WriteFile(filepath.Join(tmpDir, "main.py"), []byte(userCode), 0644); err != nil {
		return nil, err
	}

	// Mount the caller's filesystem at root and overlay the script
	// directory at /.mithras. If the filesystem can hand us a writeable
	// sys.FS (e.g. s3fs.S3FS), use that mount path so the guest's
	// writes reach the backing store; otherwise fall back to the
	// read-only WithFSMount adapter.
	fsConfig := wazero.NewFSConfig()
	if m, ok := fsys.(wazeroMountable); ok {
		fsConfig = fsConfig.(sysfs.FSConfig).WithSysFSMount(m.AsWazeroFS(), "/")
	} else {
		fsConfig = fsConfig.WithFSMount(fsys, "/")
	}
	fsConfig = fsConfig.WithDirMount(tmpDir, "/.mithras")

	config := wazero.NewModuleConfig().
		// stdio
		WithStdout(fout).
		WithStderr(ferr).
		WithStdin(fin).
		// argv
		WithArgs("python", mainPyPath).
		WithName("python").
		// fs / system
		WithFSConfig(fsConfig).
		WithSysNanosleep().
		WithSysNanotime().
		WithSysWalltime()

	mod, err := r.InstantiateModule(ctx, code, config)
	if err != nil {
		result := &Result{
			Stdout:        fout.String(),
			Stderr:        ferr.String(),
			PlatformError: err.Error(),
		}
		return result, err
	}

	defer func() { _ = mod.Close(ctx) }()

	return &Result{
		Stdout: fout.String(),
		Stderr: ferr.String(),
	}, nil
}

// emptyFS is a filesystem whose root directory exists but is empty. Every
// other path returns ENOENT. Mounting this at / lets the guest walk the
// root while ensuring no caller-supplied files are visible.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	if name == "." {
		return emptyDir{}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (emptyFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "." {
		return nil, nil
	}
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
}

func (emptyFS) Glob(pattern string) ([]string, error) { return nil, nil }

type emptyDir struct{}

func (emptyDir) Stat() (fs.FileInfo, error)         { return emptyDirInfo{}, nil }
func (emptyDir) Read(_ []byte) (int, error)         { return 0, &fs.PathError{Op: "read", Path: ".", Err: fs.ErrInvalid} }
func (emptyDir) Close() error                       { return nil }
func (emptyDir) ReadDir(_ int) ([]fs.DirEntry, error) { return nil, nil }

type emptyDirInfo struct{}

func (emptyDirInfo) Name() string       { return "." }
func (emptyDirInfo) Size() int64        { return 0 }
func (emptyDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0555 }
func (emptyDirInfo) ModTime() time.Time { return time.Time{} }
func (emptyDirInfo) IsDir() bool        { return true }
func (emptyDirInfo) Sys() any           { return nil }
