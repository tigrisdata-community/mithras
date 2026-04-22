package python

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
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

func TestRunReadsFromRoot(t *testing.T) {
	const want = "hello from the host filesystem"

	fsys := fstest.MapFS{
		"greeting.txt": &fstest.MapFile{Data: []byte(want)},
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
