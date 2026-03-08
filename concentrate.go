// Package concentrate compresses command output for consumption by language models.
//
// It reads a byte stream, classifies it as batch output, a watch-mode loop, or
// an interactive prompt, and produces a condensed summary via an LLM.
//
//   - Batch mode: accumulates all input, then summarises it once with the LLM.
//   - Watch mode: detected when successive bursts are structurally similar (e.g.
//     `watch`, `kubectl get pods`); each cycle pair is summarised independently.
//   - Interactive mode: detected when the tail of the input looks like a prompt
//     (password:, [y/N], etc.); input is passed through verbatim.
//
// The zero-latency path is preserved: if the LLM produces a bad summary,
// the original raw output is written to stdout instead.
package concentrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai"
)

type mode int

const (
	modeUndecided   mode = iota
	modeWatch            // repeating output — summarize each burst cycle
	modeInteractive      // interactive prompt — pass through verbatim
)

type burst struct {
	id         int
	raw        string
	normalized string
}

type watchWork struct {
	prev, curr burst
}

// Session reads a byte stream, classifies it, and produces compressed output.
// Push must be called from a single goroutine.
type Session struct {
	// Immutable after New; no lock required.
	client      genai.Provider
	question    string
	stdout      io.Writer
	isTTY       bool
	timeout     time.Duration
	idleTimeout time.Duration
	itvTimeout  time.Duration

	progressDone chan struct{} // closed to stop the progress goroutine
	watchCh      chan watchWork
	watchDone    chan struct{}

	// outMu serialises all writes to stdout.
	outMu sync.Mutex

	// mu protects the fields below.
	// Never hold mu and outMu simultaneously.
	// Generation counters (idleGen, itvGen) cheaply invalidate superseded timers.
	mu            sync.Mutex
	mode          mode
	pass          bool     // once true, bytes are forwarded directly to stdout
	raw           [][]byte // accumulated for batch mode
	burstChunks   [][]byte // bytes arriving in the current burst
	bursts        []burst
	sawRedraw     bool
	nextID        int
	rendered      map[[2]int]struct{}
	emittedWatch  bool
	idleGen       int    // increment to cancel the pending idle callback
	itvGen        int    // increment to cancel the pending interactive callback
	progressPhase string // "collecting" | "summarizing"; read by progress loop
}

// New creates a new session.
func New(client genai.Provider, question string, stdout io.Writer, isTTY bool, timeout time.Duration) *Session {
	return &Session{
		client:        client,
		question:      question,
		stdout:        stdout,
		isTTY:         isTTY,
		timeout:       timeout,
		idleTimeout:   1_200 * time.Millisecond,
		itvTimeout:    180 * time.Millisecond,
		nextID:        1,
		rendered:      make(map[[2]int]struct{}),
		progressPhase: "collecting",
		progressDone:  make(chan struct{}),
		watchCh:       make(chan watchWork, 8),
		watchDone:     make(chan struct{}),
	}
}

// Push processes an incoming chunk. Must be called from a single goroutine.
func (s *Session) Push(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	s.mu.Lock()
	if s.pass {
		s.mu.Unlock()
		s.outMu.Lock()
		s.stdout.Write(chunk) //nolint:errcheck
		s.outMu.Unlock()
		return
	}

	if s.mode != modeWatch {
		cp := make([]byte, len(chunk))
		copy(cp, chunk)
		s.raw = append(s.raw, cp)
	}
	s.burstChunks = append(s.burstChunks, chunk)
	if !s.sawRedraw {
		s.sawRedraw = hasRedrawSignal(string(chunk))
	}
	s.restartIdleTimer()
	s.restartInteractiveTimer()
	s.mu.Unlock()
}

// Finish reads r to completion, then waits for output to be written.
func (s *Session) Finish(ctx context.Context, r io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			s.Push(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}

	s.cancelTimers()

	s.mu.Lock()
	pass := s.pass
	m := s.mode
	s.closeBurst()
	s.mu.Unlock()

	if pass {
		s.StopProgress()
		return nil
	}

	if m == modeWatch {
		close(s.watchCh)
		<-s.watchDone
		return nil
	}

	s.mu.Lock()
	rawBytes := bytes.Join(s.raw, nil)
	s.mu.Unlock()

	rawText := string(rawBytes)
	if strings.TrimSpace(rawText) == "" {
		s.StopProgress()
		return nil
	}

	s.mu.Lock()
	s.progressPhase = "summarizing"
	s.mu.Unlock()

	tctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.StopProgress()

	s.outMu.Lock()
	defer s.outMu.Unlock()
	nlt := &nlTracker{Writer: s.stdout}
	msgs := genai.Messages{{Requests: []genai.Request{
		{Text: fmt.Sprintf("%s\n\nQuestion: %s", batchRules, s.question)},
		{Doc: genai.Doc{Filename: "stdout.txt", Src: strings.NewReader(normalize(rawText))}},
	}}}
	wrote, err := s.request(tctx, msgs, rawText, nlt)
	if !wrote || err != nil {
		_, werr := s.stdout.Write(rawBytes)
		return werr
	}
	if nlt.last != '\n' {
		_, werr := fmt.Fprintln(s.stdout)
		return werr
	}
	return nil
}

// restartIdleTimer schedules a callback that fires after idle silence.
// Must be called with mu held.
func (s *Session) restartIdleTimer() {
	s.idleGen++
	gen := s.idleGen
	time.AfterFunc(s.idleTimeout, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if gen != s.idleGen {
			return
		}
		s.closeBurst()
		if s.mode == modeUndecided && s.shouldPromoteWatch() {
			s.promoteWatch()
			s.scheduleWatch()
		}
	})
}

// restartInteractiveTimer schedules a passthrough if a prompt tail is detected.
// Must be called with mu held.
func (s *Session) restartInteractiveTimer() {
	if s.mode != modeUndecided {
		return
	}
	if !hasPromptTail(s.tail()) {
		s.itvGen++
		return
	}
	s.itvGen++
	gen := s.itvGen
	time.AfterFunc(s.itvTimeout, func() {
		s.mu.Lock()
		if gen != s.itvGen || s.mode != modeUndecided || !hasPromptTail(s.tail()) {
			s.mu.Unlock()
			return
		}
		s.mode = modeInteractive
		s.pass = true
		s.idleGen++
		s.itvGen++
		raw := bytes.Join(s.raw, nil)
		s.mu.Unlock()

		s.StopProgress()
		s.outMu.Lock()
		s.stdout.Write(raw) //nolint:errcheck
		s.outMu.Unlock()
	})
}

// cancelTimers invalidates all pending timer callbacks.
func (s *Session) cancelTimers() {
	s.mu.Lock()
	s.idleGen++
	s.itvGen++
	s.mu.Unlock()
}

// closeBurst finalises the current burst. Must be called with mu held.
func (s *Session) closeBurst() {
	if len(s.burstChunks) == 0 || s.pass {
		return
	}
	raw := string(bytes.Join(s.burstChunks, nil))
	s.burstChunks = nil
	if raw == "" {
		return
	}
	s.bursts = append(s.bursts, burst{
		id:         s.nextID,
		raw:        raw,
		normalized: normalize(raw),
	})
	s.nextID++
}

// tail returns the last 256 bytes of accumulated input. Must be called with mu held.
func (s *Session) tail() string {
	raw := bytes.Join(s.raw, nil)
	if len(raw) > 256 {
		raw = raw[len(raw)-256:]
	}
	return string(raw)
}

// shouldPromoteWatch returns true when the burst pattern looks like a watch loop.
// Must be called with mu held.
func (s *Session) shouldPromoteWatch() bool {
	if len(s.bursts) < 2 {
		return false
	}
	prev := s.bursts[len(s.bursts)-2]
	curr := s.bursts[len(s.bursts)-1]
	return s.sawRedraw || structuralSimilarity(prev.raw, curr.raw) >= 0.55
}

// promoteWatch switches to watch mode. Must be called with mu held.
func (s *Session) promoteWatch() {
	s.mode = modeWatch
	s.raw = nil
	s.idleGen++
	s.itvGen++
	// Signal progress stop without blocking; the goroutine clears the line.
	select {
	case <-s.progressDone:
	default:
		close(s.progressDone)
	}
	go s.watchWorker()
}

// scheduleWatch enqueues a watch summary for the latest two bursts.
// Must be called with mu held.
func (s *Session) scheduleWatch() {
	if len(s.bursts) < 2 {
		return
	}
	prev := s.bursts[len(s.bursts)-2]
	curr := s.bursts[len(s.bursts)-1]
	key := [2]int{prev.id, curr.id}
	if _, seen := s.rendered[key]; seen {
		return
	}
	s.rendered[key] = struct{}{}
	s.watchCh <- watchWork{prev: prev, curr: curr}
}

// watchWorker processes watch summaries sequentially from watchCh.
func (s *Session) watchWorker() {
	defer close(s.watchDone)
	for work := range s.watchCh {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		msgs := genai.Messages{{Requests: []genai.Request{
			{Text: fmt.Sprintf("%s\n\nQuestion: %s", watchRules, s.question)},
			{Doc: genai.Doc{Filename: "previous.txt", Src: strings.NewReader(work.prev.normalized)}},
			{Doc: genai.Doc{Filename: "current.txt", Src: strings.NewReader(work.curr.normalized)}},
		}}}

		s.outMu.Lock()
		if s.isTTY {
			_, _ = fmt.Fprintf(s.stdout, "\x1b[2J\x1b[H")
		} else {
			s.mu.Lock()
			emitted := s.emittedWatch
			s.emittedWatch = true
			s.mu.Unlock()
			if emitted {
				_, _ = fmt.Fprintln(s.stdout)
			}
		}
		nlt := &nlTracker{Writer: s.stdout}
		wrote, err := s.request(ctx, msgs, work.curr.raw, nlt)
		if wrote && nlt.last != '\n' {
			_, _ = fmt.Fprintln(s.stdout)
		}
		s.outMu.Unlock()
		cancel()

		if err != nil || !wrote {
			s.watchFallback(work.curr.raw)
			for range s.watchCh { // drain
			}
			return
		}
		s.mu.Lock()
		if len(s.bursts) > 2 {
			s.bursts = s.bursts[len(s.bursts)-2:]
		}
		s.mu.Unlock()
	}
}

func (s *Session) watchFallback(raw string) {
	s.mu.Lock()
	s.mode = modeInteractive
	s.pass = true
	s.mu.Unlock()
	s.StopProgress()
	s.outMu.Lock()
	_, _ = fmt.Fprint(s.stdout, raw)
	s.outMu.Unlock()
}

// Flush acquires and releases outMu, establishing a happens-before relationship
// with respect to all prior stdout writes. Useful in tests.
func (s *Session) Flush() {
	s.outMu.Lock()
	s.outMu.Unlock() //nolint:staticcheck
}

var (
	spinFrames = [4]string{"-", "\\", "|", "/"}
	dotFrames  = [6]string{"", ".", "..", "...", "..", "."}
)

func (s *Session) StopProgress() {
	select {
	case <-s.progressDone:
	default:
		close(s.progressDone)
	}
}

func (s *Session) ProgressLoop(stderr io.Writer) {
	defer fmt.Fprint(stderr, "\r\x1b[2K") //nolint:errcheck
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	labels := map[string]string{
		"collecting":  "concentrate: waiting",
		"summarizing": "concentrate: summarizing",
	}
	i := 0
	for {
		select {
		case <-s.progressDone:
			return
		case <-ticker.C:
			s.mu.Lock()
			active := !s.pass && s.mode != modeWatch
			phase := s.progressPhase
			s.mu.Unlock()
			if !active {
				continue
			}
			frame := spinFrames[i%4]
			dots := dotFrames[(i/4)%6]
			label := labels[phase]
			_, _ = fmt.Fprintf(stderr, "\r\x1b[2K%s %s%s", frame, label, dots)
			i++
		}
	}
}

const commonRules = `Rules:
- Answer only what the question asks, in the same language as the question.
- No markdown. Prefer one sentence; never exceed three short lines.
- Never ask for more input.`

const batchRules = `You are a filter in a shell pipeline. Summarise the attached command output to answer the question.
` + commonRules + `
- If the output is insufficient to answer, reply only with "concise: insufficient output." in the same language as the question.
- If the output is already shorter than your summary would be, quote it directly.`

const watchRules = `You are a filter in a shell pipeline. The attached files show two consecutive cycles of a repeating command. Answer the question based on what changed.
` + commonRules + `
- If nothing relevant changed, reply only with "No change." in the same language as the question.`

// nlTracker wraps a writer, tracking the last byte written for trailing-newline detection.
type nlTracker struct {
	io.Writer
	last byte
}

func (w *nlTracker) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.last = p[n-1]
	}
	return n, err
}

// qualityCommitLen is the number of buffered bytes after which the output is
// committed and subsequent tokens are streamed live without further gating.
const qualityCommitLen = 200

// qualityGate buffers LLM output tokens and checks quality before writing to w.
// Once the buffer exceeds qualityCommitLen without a bad signal, it flushes and
// switches to live streaming. If a bad signal is detected before commit, the
// caller should discard the response and fall back to raw output.
type qualityGate struct {
	w      io.Writer
	source string
	buf    strings.Builder
	live   bool
}

// write appends s to the buffer (or streams it live if already committed).
// Returns bad=true if the accumulated output looks like a bad summary.
func (q *qualityGate) write(s string) (bad bool, err error) {
	if q.live {
		_, err = io.WriteString(q.w, s)
		return false, err
	}
	q.buf.WriteString(s)
	if isBadSummary(q.source, q.buf.String()) {
		return true, nil
	}
	if q.buf.Len() >= qualityCommitLen {
		_, err = io.WriteString(q.w, q.buf.String())
		q.buf.Reset()
		q.live = true
	}
	return false, err
}

// flush writes any remaining buffered output after the stream ends.
// Returns (wrote, err): wrote is false if the buffer was empty or bad.
func (q *qualityGate) flush() (bool, error) {
	if q.live {
		return true, nil
	}
	text := q.buf.String()
	if text == "" || isBadSummary(q.source, text) {
		return false, nil
	}
	_, err := io.WriteString(q.w, text)
	return err == nil, err
}

// request streams the LLM response to w, gating on quality.
// Returns (wrote, err): wrote is false if no useful output was produced,
// in which case the caller should fall back to raw output.
func (s *Session) request(ctx context.Context, msgs genai.Messages, source string, w io.Writer) (bool, error) {
	opts := []genai.GenOption{
		&genai.GenOptionText{Temperature: 0.1, MaxTokens: 80},
	}
	stream, getResult := s.client.GenStream(ctx, msgs, opts...)
	qg := &qualityGate{w: w, source: source}
	for reply := range stream {
		if reply.Text == "" {
			continue
		}
		if bad, err := qg.write(reply.Text); bad {
			for range stream {} // drain
			getResult()        //nolint:errcheck
			return false, nil
		} else if err != nil {
			return qg.live, err
		}
	}
	if _, err := getResult(); err != nil {
		return qg.live, err
	}
	return qg.flush()
}

