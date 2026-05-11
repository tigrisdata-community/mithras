package python

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
)

func TestRunWritesToRoot(t *testing.T) {
	t.Parallel()
	const payload = "python says hi"

	fsys := memfs.New()

	code := `with open("/out.txt", "w") as f:
    f.write("` + payload + `")`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := Run(ctx, fsys, code)
	if err != nil {
		t.Logf("stdout: %s", res.Stdout)
		t.Logf("stderr: %s", res.Stderr)
		t.Logf("platform error: %s", res.PlatformError)
		t.Fatal(err)
	}

	f, err := fsys.Open("out.txt")
	if err != nil {
		t.Logf("stderr: %s", res.Stderr)
		t.Fatalf("python did not create /out.txt on the host fs: %v", err)
	}
	defer func() { _ = f.Close() }()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read out.txt: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("payload mismatch:\nwant=%q\ngot =%q", payload, got)
	}
}
