package python

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
)

func TestRun(t *testing.T) {
	var code = `import sys

print(f"Python {sys.version} running in {sys.platform}/wazero.")`

	res, err := Run(context.Background(), nil, code)
	if err != nil {
		t.Logf("stdout: %s", res.Stdout)
		t.Logf("stderr: %s", res.Stderr)
		t.Logf("platform error: %s", res.PlatformError)
		t.Fatal(err)
	}

	t.Logf("stdout: %s", res.Stdout)
	t.Logf("stderr: %s", res.Stderr)
}

// TestRunListsRoot asserts that listing the mounted root from inside the
// guest returns the entries seeded on the host. If kefka's billyfs adapter
// ever changes how it dispatches directory operations, this regresses
// before the python tool stops being able to enumerate the bucket.
func TestRunListsRoot(t *testing.T) {
	fsys := memfs.New()
	for _, name := range []string{"alpha.txt", "beta.txt"} {
		if err := util.WriteFile(fsys, name, []byte("x"), 0644); err != nil {
			t.Fatalf("seed memfs: %v", err)
		}
	}

	code := `import os
for name in sorted(os.listdir("/")):
    print(name)`

	res, err := Run(context.Background(), fsys, code)
	if err != nil {
		t.Logf("stdout: %s", res.Stdout)
		t.Logf("stderr: %s", res.Stderr)
		t.Logf("platform error: %s", res.PlatformError)
		t.Fatal(err)
	}

	for _, want := range []string{"alpha.txt", "beta.txt"} {
		if !strings.Contains(res.Stdout, want) {
			t.Logf("stdout: %s", res.Stdout)
			t.Logf("stderr: %s", res.Stderr)
			t.Errorf("os.listdir output missing %q", want)
		}
	}
}

func TestRunReadsFromRoot(t *testing.T) {
	const want = "hello from the host filesystem"

	fsys := memfs.New()
	if err := util.WriteFile(fsys, "greeting.txt", []byte(want), 0644); err != nil {
		t.Fatalf("seed memfs: %v", err)
	}

	code := `with open("/greeting.txt") as f:
    print(f.read(), end="")`

	res, err := Run(context.Background(), fsys, code)
	if err != nil {
		t.Logf("stdout: %s", res.Stdout)
		t.Logf("stderr: %s", res.Stderr)
		t.Logf("platform error: %s", res.PlatformError)
		t.Fatal(err)
	}

	if got := strings.TrimSpace(res.Stdout); got != want {
		t.Logf("stderr: %s", res.Stderr)
		t.Logf("want: %q", want)
		t.Logf("got:  %q", got)
		t.Error("python did not read the expected contents from /")
	}
}
