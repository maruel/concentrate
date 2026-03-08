package concentrate

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/scoreboard"
)

// mockProvider implements genai.Provider for testing.
type mockProvider struct {
	result string
	err    error
	calls  int
}

func (m *mockProvider) GenSync(_ context.Context, _ genai.Messages, _ ...genai.GenOption) (genai.Result, error) {
	return genai.Result{}, nil
}

func (m *mockProvider) Name() string                             { return "mock" }
func (m *mockProvider) ModelID() string                          { return "" }
func (m *mockProvider) OutputModalities() genai.Modalities       { return nil }
func (m *mockProvider) Capabilities() genai.ProviderCapabilities { return genai.ProviderCapabilities{} }
func (m *mockProvider) Scoreboard() scoreboard.Score             { return scoreboard.Score{} }
func (m *mockProvider) HTTPClient() *http.Client                 { return nil }
func (m *mockProvider) GenStream(_ context.Context, _ genai.Messages, _ ...genai.GenOption) (iter.Seq[genai.Reply], func() (genai.Result, error)) {
	m.calls++
	return func(yield func(genai.Reply) bool) {
		if m.err == nil {
			yield(genai.Reply{Text: m.result})
		}
	}, func() (genai.Result, error) {
		return genai.Result{}, m.err
	}
}
func (m *mockProvider) ListModels(_ context.Context) ([]genai.Model, error) { return nil, nil }
func (m *mockProvider) GenAsync(_ context.Context, _ genai.Messages, _ ...genai.GenOption) (genai.Job, error) {
	return "", nil
}
func (m *mockProvider) PokeResult(_ context.Context, _ genai.Job) (genai.Result, error) {
	return genai.Result{}, nil
}
func (m *mockProvider) CacheAddRequest(_ context.Context, _ genai.Messages, _, _ string, _ time.Duration, _ ...genai.GenOption) (string, error) {
	return "", nil
}
func (m *mockProvider) CacheList(_ context.Context) ([]genai.CacheEntry, error) { return nil, nil }
func (m *mockProvider) CacheDelete(_ context.Context, _ string) error            { return nil }

func newSession(mock *mockProvider) (*Session, *bytes.Buffer) {
	var buf bytes.Buffer
	sess := New(mock, "", &buf, false, time.Minute)
	// Speed up timers for tests
	sess.idleTimeout = 30 * time.Millisecond
	sess.itvTimeout = 20 * time.Millisecond
	return sess, &buf
}

func TestConcentrator(t *testing.T) {
	t.Run("batch summarizes", func(t *testing.T) {
		mock := &mockProvider{result: "all tests passed"}
		sess, buf := newSession(mock)

		if err := sess.Finish(t.Context(), strings.NewReader(strings.Repeat("lots of test output\n", 10))); err != nil {
			t.Fatal(err)
		}

		if got := buf.String(); got != "all tests passed\n" {
			t.Errorf("got %q; want summary", got)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		mock := &mockProvider{result: "summary"}
		sess, buf := newSession(mock)

		if err := sess.Finish(t.Context(), strings.NewReader("")); err != nil {
			t.Fatal(err)
		}

		if buf.Len() != 0 {
			t.Errorf("expected empty output for empty input; got %q", buf.String())
		}
		if mock.calls != 0 {
			t.Error("provider should not be called for empty input")
		}
	})

	t.Run("fallback on error", func(t *testing.T) {
		mock := &mockProvider{err: fmt.Errorf("connection refused")}
		sess, buf := newSession(mock)

		input := "raw output\n"
		if err := sess.Finish(t.Context(), strings.NewReader(input)); err != nil {
			t.Fatal(err)
		}

		if buf.String() != input {
			t.Errorf("got %q; want raw passthrough", buf.String())
		}
	})

	t.Run("interactive passthrough", func(t *testing.T) {
		mock := &mockProvider{result: "summary"}
		sess, buf := newSession(mock)

		sess.Push([]byte("Are you sure? [y/N] "))
		time.Sleep(50 * time.Millisecond) // wait for interactive timer

		sess.mu.Lock()
		isPass := sess.pass
		sess.mu.Unlock()

		if !isPass {
			t.Error("expected passthrough mode after prompt detection")
		}
		// Synchronize with the timer goroutine's write before reading the buffer.
		sess.Flush()

		if !strings.Contains(buf.String(), "[y/N]") {
			t.Errorf("expected raw output in passthrough mode; got %q", buf.String())
		}

		sess.Finish(t.Context(), strings.NewReader("")) //nolint:errcheck
	})

	t.Run("watch mode detected", func(t *testing.T) {
		mock := &mockProvider{result: "watch summary"}
		sess, buf := newSession(mock)

		// Two structurally similar bursts → watch mode
		burstLine := func(n int) []byte {
			return []byte(fmt.Sprintf("Tasks: %d pending\nStatus: running\nUptime: %ds\n", n, n*2))
		}

		sess.Push(burstLine(1))
		time.Sleep(50 * time.Millisecond) // idle timer fires, closes burst 1

		sess.Push(burstLine(2))
		time.Sleep(50 * time.Millisecond) // idle timer fires, closes burst 2 → watch promoted

		if err := sess.Finish(t.Context(), strings.NewReader("")); err != nil {
			t.Fatal(err)
		}

		if mock.calls == 0 {
			t.Error("expected provider to be called for watch summary")
		}
		if !strings.Contains(buf.String(), "watch summary") {
			t.Errorf("expected watch summary in output; got %q", buf.String())
		}
	})
}
