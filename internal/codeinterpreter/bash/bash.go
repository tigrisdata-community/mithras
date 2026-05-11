// Package bash runs shell scripts against a billy.Filesystem using
// mvdan.cc/sh as the parser/interpreter and kefka's command registry for
// coreutils. The shell sees the supplied billy.Filesystem rooted at /; file
// reads, directory listings, and pwd/cd are routed through the registry so
// the script cannot escape the supplied fs.
package bash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
	"tangled.org/xeiaso.net/kefka/command/registry"
	"tangled.org/xeiaso.net/kefka/command/registry/coreutils"
)

// ErrEmptyScript is returned by [Run] when the supplied script is empty.
var ErrEmptyScript = errors.New("bash: empty script")

// Result is the outcome of executing a bash script.
type Result struct {
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	ExitCode      int    `json:"exitCode"`
	PlatformError string `json:"platformError,omitempty"`
}

// Run parses script as bash and executes it with fsys mounted at /. A nil
// fsys is replaced with an empty in-memory billy filesystem so the script
// sees a clean root. Stdout and stderr are captured into the returned
// [Result]; a non-zero shell exit status is reported via ExitCode rather
// than the returned error.
func Run(ctx context.Context, fsys billy.Filesystem, script string) (*Result, error) {
	if strings.TrimSpace(script) == "" {
		return nil, ErrEmptyScript
	}

	if fsys == nil {
		fsys = memfs.New()
	}

	prog, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "<bash>")
	if err != nil {
		return nil, fmt.Errorf("can't parse bash: %w", err)
	}

	reg := registry.New()
	coreutils.Register(reg)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	var sh *interp.Runner

	exec := func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			return reg.Exec(ctx, fsys, sh, args)
		}
	}

	env := expand.ListEnviron(
		"HOME=/",
		"IFS= \t\n",
		"PWD=/",
		"OLDPWD=/",
		"OPTIND=1",
		"KEFKA=1",
		"PATH=/usr/bin:/bin",
	)

	sh, err = interp.New(
		interp.Env(env),
		interp.StdIO(nil, stdout, stderr),
		interp.ExecHandlers(exec),
		interp.CallHandler(callHandler(reg, fsys, stdout, stderr)),
		interp.StatHandler(statHandler(reg, fsys)),
		interp.OpenHandler(openHandler(reg, fsys)),
		interp.ReadDirHandler2(readDirHandler(reg, fsys)),
	)
	if err != nil {
		return nil, fmt.Errorf("can't make shell: %w", err)
	}

	result := &Result{}

	runErr := sh.Run(ctx, prog)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if runErr != nil {
		if status, ok := errors.AsType[interp.ExitStatus](runErr); ok {
			result.ExitCode = int(status)
		} else {
			result.PlatformError = runErr.Error()
			return result, runErr
		}
	}

	return result, nil
}

// statHandler routes stat through the registry's pwd-aware resolver so the
// shell sees fsys-relative paths instead of falling back to interp's default
// host-rooted handler.
func statHandler(reg *registry.Impl, fsys billy.Filesystem) interp.StatHandlerFunc {
	return func(ctx context.Context, name string, followSymlinks bool) (fs.FileInfo, error) {
		resolved := reg.Resolve(name)
		if !followSymlinks {
			if r, ok := fsys.(billy.Symlink); ok {
				return r.Lstat(resolved)
			}
		}
		return fsys.Stat(resolved)
	}
}

// openHandler exposes fsys as read-only to the shell — write/create/append
// flags are rejected so redirections like `>foo` cannot mutate the
// underlying fs. Tool-mediated writes still go through coreutils registered
// commands.
func openHandler(reg *registry.Impl, fsys billy.Filesystem) interp.OpenHandlerFunc {
	return func(ctx context.Context, name string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
			return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
		}
		f, err := fsys.Open(reg.Resolve(name))
		if err != nil {
			return nil, err
		}
		return readOnlyFile{f}, nil
	}
}

func readDirHandler(reg *registry.Impl, fsys billy.Filesystem) interp.ReadDirHandlerFunc2 {
	return func(ctx context.Context, name string) ([]fs.DirEntry, error) {
		entries, err := fsys.ReadDir(reg.Resolve(name))
		if err != nil {
			return nil, err
		}
		out := make([]fs.DirEntry, len(entries))
		for i, e := range entries {
			out[i] = fs.FileInfoToDirEntry(e)
		}
		return out, nil
	}
}

type readOnlyFile struct{ billy.File }

func (readOnlyFile) Write([]byte) (int, error) { return 0, fs.ErrPermission }

// callHandler intercepts cd and pwd before interp's builtins so directory
// state is owned by the registry's fsys-relative pwd instead of interp's
// host-rooted Dir. Intercepted calls are replaced with `:` so interp's
// builtin doesn't run.
func callHandler(reg *registry.Impl, fsys billy.Filesystem, stdout, stderr io.Writer) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) == 0 {
			return args, nil
		}
		switch args[0] {
		case "cd":
			target := ""
			if len(args) > 1 {
				target = args[1]
			}
			if err := reg.Chdir(fsys, target); err != nil {
				fmt.Fprintln(stderr, err)
				return []string{"false"}, nil
			}
			return []string{":"}, nil
		case "pwd":
			pwd := reg.Pwd()
			if pwd == "." {
				fmt.Fprintln(stdout, "/")
			} else {
				fmt.Fprintln(stdout, "/"+pwd)
			}
			return []string{":"}, nil
		}
		return args, nil
	}
}
