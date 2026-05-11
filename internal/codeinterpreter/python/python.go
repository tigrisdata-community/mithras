package python

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"tangled.org/xeiaso.net/kefka/wasm/billyfs"
)

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
// guest filesystem. The caller's billy.Filesystem is mounted at /, and a
// host temp directory holding this file is overlaid at /.mithras.
const mainPyPath = "/.mithras/main.py"

// Run executes userCode inside a Python WASI sandbox. The caller-supplied
// fsys is mounted at the guest root; if nil, an in-memory billy filesystem
// is used so the guest sees an empty /.
func Run(ctx context.Context, fsys billy.Filesystem, userCode string) (*Result, error) {
	fout := &bytes.Buffer{}
	ferr := &bytes.Buffer{}
	fin := &bytes.Buffer{}

	if fsys == nil {
		fsys = memfs.New()
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

	// Mount the caller's billy filesystem at root via kefka's billyfs
	// adapter (which implements wazero's experimental/sys.FS), and overlay
	// the script directory at /.mithras. The dirAwareFS shim teaches the
	// underlying billy filesystem to satisfy OpenFile(".", O_RDONLY) — the
	// call wazero issues to materialize the preopen — instead of failing
	// the way memfs and other directory-rejecting billy backends do.
	fsConfig := wazero.NewFSConfig().(sysfs.FSConfig).
		WithSysFSMount(billyfs.New(dirAwareFS{Filesystem: fsys}), "/").
		WithDirMount(tmpDir, "/.mithras")

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

// dirAwareFS wraps a billy.Filesystem so OpenFile on a directory returns a
// minimal billy.File instead of erroring. The underlying call paths (memfs,
// chroot helper, etc.) refuse directory opens through OpenFile; wazero's WASI
// preopen plumbing relies on OpenFile(".", O_RDONLY) succeeding to bind the
// mount handle, so we intercept that case and synthesize a directory file.
type dirAwareFS struct {
	billy.Filesystem
}

func (d dirAwareFS) Open(filename string) (billy.File, error) {
	return d.OpenFile(filename, os.O_RDONLY, 0)
}

func (d dirAwareFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) == 0 {
		if info, err := d.Stat(filename); err == nil && info.IsDir() {
			return &dirFile{name: filename}, nil
		}
	}
	return d.Filesystem.OpenFile(filename, flag, perm)
}

var errIsDir = errors.New("is a directory")

// dirFile is the placeholder billy.File handed back for directory opens. The
// kefka billyfs adapter only consults the file's Name() to satisfy Stat,
// IsDir, and Readdir on the underlying filesystem — it never reads or writes
// a directory handle directly — so I/O methods can safely return errors.
type dirFile struct {
	name string
}

func (f *dirFile) Name() string                          { return f.name }
func (f *dirFile) Read(_ []byte) (int, error)            { return 0, errIsDir }
func (f *dirFile) Write(_ []byte) (int, error)           { return 0, errIsDir }
func (f *dirFile) ReadAt(_ []byte, _ int64) (int, error) { return 0, errIsDir }
func (f *dirFile) Seek(_ int64, _ int) (int64, error)    { return 0, errIsDir }
func (f *dirFile) Close() error                          { return nil }
func (f *dirFile) Lock() error                           { return nil }
func (f *dirFile) Unlock() error                         { return nil }
func (f *dirFile) Truncate(_ int64) error                { return errIsDir }
