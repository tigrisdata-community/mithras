package webhook

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubRunner is a [Runner] that records each call and optionally blocks on
// ctx.Done() so tests can drive shutdown semantics without standing up a full
// agentloop.
type stubRunner struct {
	calls    atomic.Int64
	block    bool
	lastCtx  atomic.Value // context.Context
	lastID   atomic.Value // string
	lastText atomic.Value // string
	done     chan struct{}
}

func newStubRunner(block bool) *stubRunner {
	return &stubRunner{block: block, done: make(chan struct{})}
}

func (s *stubRunner) Run(ctx context.Context, requestID, prompt string) {
	s.calls.Add(1)
	s.lastCtx.Store(ctx)
	s.lastID.Store(requestID)
	s.lastText.Store(prompt)
	if s.block {
		<-ctx.Done()
		close(s.done)
	}
}

func TestBackgroundLauncherLaunch(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		launches  int
		requestID string
		prompt    string
	}{
		{name: "single launch", launches: 1, requestID: "req-1", prompt: "hello"},
		{name: "repeated launches", launches: 3, requestID: "req-n", prompt: "ping"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := newStubRunner(false)
			var wg sync.WaitGroup
			launcher := NewBackgroundLauncher(context.Background(), stub, &wg)

			for range tt.launches {
				launcher.Launch(tt.requestID, tt.prompt)
			}
			wg.Wait()

			if got := stub.calls.Load(); got != int64(tt.launches) {
				t.Errorf("runner.Run call count = %d, want %d", got, tt.launches)
			}

			if id, _ := stub.lastID.Load().(string); id != tt.requestID {
				t.Errorf("runner.Run requestID = %q, want %q", id, tt.requestID)
			}
			if text, _ := stub.lastText.Load().(string); text != tt.prompt {
				t.Errorf("runner.Run prompt = %q, want %q", text, tt.prompt)
			}
		})
	}
}

func TestBackgroundLauncherPropagatesRootContext(t *testing.T) {
	t.Parallel()

	type ctxKey string
	const key ctxKey = "root-marker"

	for _, tt := range []struct {
		name   string
		marker string
	}{
		{name: "value-propagated", marker: "alpha"},
		{name: "different value propagated", marker: "beta"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := context.WithValue(context.Background(), key, tt.marker)

			stub := newStubRunner(false)
			var wg sync.WaitGroup
			launcher := NewBackgroundLauncher(root, stub, &wg)

			launcher.Launch("req-ctx", "prompt")
			wg.Wait()

			gotCtx, _ := stub.lastCtx.Load().(context.Context)
			if gotCtx == nil {
				t.Fatalf("runner.Run ctx was nil")
			}
			if err := gotCtx.Err(); err != nil {
				t.Errorf("runner.Run ctx.Err() = %v, want nil", err)
			}
			if got, _ := gotCtx.Value(key).(string); got != tt.marker {
				t.Errorf("runner.Run ctx value = %q, want %q", got, tt.marker)
			}
		})
	}
}

func TestBackgroundLauncherShutdownCancelsRunner(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		launches int
	}{
		{name: "single in-flight cancelled", launches: 1},
		{name: "multiple in-flight cancelled", launches: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root, cancel := context.WithCancel(context.Background())

			stubs := make([]*stubRunner, tt.launches)
			var wg sync.WaitGroup
			for i := range tt.launches {
				stubs[i] = newStubRunner(true)
				launcher := NewBackgroundLauncher(root, stubs[i], &wg)
				launcher.Launch("req", "prompt")
			}

			// Wait for every stub to have observed Run at least once before
			// cancelling, so the test exercises cancellation during an
			// in-flight invocation rather than a race before Run starts.
			deadline := time.Now().Add(2 * time.Second)
			for _, s := range stubs {
				for s.calls.Load() == 0 {
					if time.Now().After(deadline) {
						t.Fatalf("runner never started")
					}
					time.Sleep(time.Millisecond)
				}
			}

			cancel()

			waited := make(chan struct{})
			go func() {
				wg.Wait()
				close(waited)
			}()

			select {
			case <-waited:
			case <-time.After(2 * time.Second):
				t.Fatalf("wg.Wait did not complete after root cancel")
			}

			for i, s := range stubs {
				select {
				case <-s.done:
				case <-time.After(time.Second):
					t.Errorf("stub[%d] never observed ctx cancellation", i)
				}
			}
		})
	}
}
