package bash

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
)

func TestRun(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		script       string
		seed         map[string]string
		wantStdout   string
		wantExitCode int
		wantErr      error
	}{
		{
			name:       "echo hello",
			script:     "echo hello",
			wantStdout: "hello\n",
		},
		{
			name:       "coreutils cat reads seeded file",
			script:     "cat /greeting.txt",
			seed:       map[string]string{"greeting.txt": "hi there"},
			wantStdout: "hi there",
		},
		{
			name:       "ls lists seeded entries",
			script:     "ls /",
			seed:       map[string]string{"alpha.txt": "a", "beta.txt": "b"},
			wantStdout: "alpha.txt\nbeta.txt\n",
		},
		{
			name:         "non-zero exit captured in ExitCode",
			script:       "false",
			wantExitCode: 1,
		},
		{
			name:    "empty script is rejected",
			script:  "   \n  ",
			wantErr: ErrEmptyScript,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := memfs.New()
			for path, body := range tt.seed {
				if err := util.WriteFile(fsys, path, []byte(body), 0644); err != nil {
					t.Fatalf("seed %s: %v", path, err)
				}
			}

			res, err := Run(context.Background(), fsys, tt.script)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Logf("stdout: %q", res.Stdout)
				t.Logf("stderr: %q", res.Stderr)
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantExitCode != res.ExitCode {
				t.Errorf("exitCode = %d, want %d", res.ExitCode, tt.wantExitCode)
			}

			if tt.wantStdout != "" && !strings.Contains(res.Stdout, tt.wantStdout) {
				t.Logf("stderr: %q", res.Stderr)
				t.Errorf("stdout = %q, want substring %q", res.Stdout, tt.wantStdout)
			}
		})
	}
}

func TestRunReadOnlyRedirect(t *testing.T) {
	t.Parallel()

	res, err := Run(context.Background(), memfs.New(), "echo hi > /out.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.ExitCode == 0 {
		t.Errorf("expected non-zero exit when redirecting to fs, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
}
