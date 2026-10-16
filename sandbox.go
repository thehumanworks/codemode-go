// Package codemode runs agent-written JavaScript in a QuickJS sandbox
// (compiled to WebAssembly, executed by wazero, no cgo) and bridges the
// script's calls to Go functions.
//
// It implements the "code mode" pattern that Cloudflare's codemode package
// and pi's codemode extension established: instead of calling tools one at
// a time, a model writes one script that calls them as async methods of a
// namespace object, combines the results in code, and returns a summary.
// Only the return value and console output come back to the model.
//
// A script sees:
//
//   - a namespace object (Options.Namespace, default "tools") whose methods
//     are the Tools given to Run; each takes one argument and returns a
//     Promise of the host function's JSON result;
//   - any extra globals, as async functions taking positional arguments
//     (package catalog provides search and describe);
//   - console.log and friends, captured for the host;
//   - plain ECMAScript. The quickjs-libc helpers the underlying qjs build
//     ships (std, os, print, setTimeout and friends) are removed from the
//     global object; the VM's filesystem root is an empty read-only
//     directory, its stdout and stderr are discarded, and it has no network
//     and no environment. A dynamic import("qjs:os") can still reach the libc
//     module; the README lists what that leaves open (a blocking sleep is the
//     one that matters, and Run still returns on time).
//
// The code runs as the body of an async function, so it can use await at the
// top level and return a value. Independent calls run concurrently with
// Promise.all. Limits on wall-clock time, heap, host calls, concurrency and
// console size bound every run.
package codemode

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fastschema/qjs"
)

//go:embed prelude.js
var preludeSource string

// HostFunc handles one call from a script. args is the JSON the script
// passed: an object for tools, an array of positional arguments for
// globals; it is always valid JSON. The returned JSON becomes the resolved
// value ("null" when empty); an error rejects the promise with the error's
// message.
//
// The context is cancelled when the script finishes, fails, or times out.
// A HostFunc that blocks must honour it: Run waits only briefly for host
// calls after the script ends, and a call that ignores its context leaks.
type HostFunc func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

// Tool is one method of the script's namespace object.
type Tool struct {
	// Name is the host name, for example rex_capture. Scripts can call it
	// as is (ns.rex_capture) and through JSName.
	Name string
	// JSName is the identifier scripts normally use, for example capture.
	JSName string
	Call   HostFunc
}

// Options bound a script run. The zero value means the defaults.
type Options struct {
	// Namespace is the global object holding the tools. Default "tools".
	Namespace string
	// Timeout is the wall-clock limit for the whole script. Default 60s.
	// An earlier deadline on the context wins.
	Timeout time.Duration
	// MemoryLimit caps the VM heap in bytes. Default 64 MiB.
	MemoryLimit int
	// MaxCalls caps host calls per script. Default 200.
	MaxCalls int
	// MaxConcurrency caps host calls in flight. Default 8.
	MaxConcurrency int
	// MaxLogChars caps captured console output. Default 64 KiB. Once the
	// cap is reached, later lines are dropped and a marker line says so.
	MaxLogChars int
	// Hint is appended to the error a script gets for an unknown namespace
	// member, after the did-you-mean suggestions. Hosts that provide
	// discovery globals say so here (catalog.Hint builds one).
	Hint string
	// OnLog receives each console line the sandbox keeps, as the script
	// prints it; lines dropped by MaxLogChars never reach it. It runs on the
	// script's goroutine, so it must return quickly. Optional.
	OnLog func(line string)
}

func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = "tools"
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	if o.MemoryLimit <= 0 {
		o.MemoryLimit = 64 << 20
	}
	if o.MaxCalls <= 0 {
		o.MaxCalls = 200
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = 8
	}
	if o.MaxLogChars <= 0 {
		o.MaxLogChars = 64 << 10
	}
	return o
}

// Output is what a finished script produced.
type Output struct {
	// Result is the JSON of the script's return value; "null" when none.
	Result json.RawMessage
	// Logs are the console lines, in order, capped by Options.MaxLogChars.
	Logs []string
	// Calls counts host calls the script made, including failed ones.
	Calls   int
	Elapsed time.Duration
}

// ScriptError is a failure inside the script: a thrown value, a syntax
// error, a stall, running out of memory, exceeding the call limit, or the
// VM stopping unexpectedly. Logs hold what the script printed before
// failing.
type ScriptError struct {
	Message string
	Logs    []string
	Calls   int
	Elapsed time.Duration
}

func (e *ScriptError) Error() string { return e.Message }

// ErrTimeout is the sentinel a TimeoutError unwraps to.
var ErrTimeout = errors.New("script timed out")

// TimeoutError reports that the script exceeded its wall-clock limit. Logs
// printed before the deadline are kept.
type TimeoutError struct {
	// Timeout is the limit that applied: Options.Timeout, or the time that
	// was left on the context when it was shorter.
	Timeout time.Duration
	Logs    []string
	Calls   int
	Elapsed time.Duration
}

func (e *TimeoutError) Error() string {
	msg := "script timed out after " + e.Timeout.String()
	if e.Calls > 0 {
		msg += "; any tool call still running was cancelled"
	}
	return msg
}

// Unwrap makes errors.Is(err, ErrTimeout) true.
func (e *TimeoutError) Unwrap() error { return ErrTimeout }

var (
	warmOnce sync.Once
	// warmErr is set when the sandbox cannot be trusted; Run then refuses.
	warmErr error
	// sandboxRoot is the empty read-only directory the VM sees as "/".
	sandboxRoot string
	// vmStopGrace is how long Run waits for the VM to stop after the
	// deadline before returning without it (a libc sleep cannot be
	// interrupted). A variable so tests can shorten it.
	vmStopGrace = time.Second
)

// Warm prepares the sandbox so the first script does not pay for it (about
// 300ms): it compiles the QuickJS module, creates the empty directory the
// VM sees as its filesystem, and checks that a runaway script can be
// stopped. It is safe to call from several goroutines and more than once;
// Run calls it, and hosts typically call it in a goroutine at start-up.
//
// qjs compiles the module once per process and bakes CloseOnContextDone (the
// mechanism behind the timeout) into that compilation. Warm forces a
// compilation with the flag set, so a runtime another library created
// without it cannot disable timeouts here; if the check still fails, every
// Run returns an error instead of running scripts without a deadline.
func Warm() { warmOnce.Do(warm) }

func warm() {
	dir, err := os.MkdirTemp("", "codemode-sandbox-")
	if err != nil {
		warmErr = fmt.Errorf("create sandbox root: %w", err)
		return
	}
	_ = os.Chmod(dir, 0o555)
	sandboxRoot = dir

	rt, err := qjs.New(vmOption(context.Background(), 16<<20, true))
	if err != nil {
		warmErr = fmt.Errorf("compile sandbox: %w", err)
		return
	}
	rt.Close()

	// Self-test: a busy loop with a short deadline must stop.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		rt, err := qjs.New(vmOption(ctx, 16<<20, false))
		if err != nil {
			return
		}
		defer func() {
			defer func() { _ = recover() }()
			rt.Close()
		}()
		_, _ = rt.Context().Eval("warm.js", qjs.Code("for(;;){}"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		warmErr = errors.New("the sandbox cannot interrupt scripts (the qjs module in use was compiled without CloseOnContextDone)")
	}
}

func vmOption(ctx context.Context, memory int, recompile bool) qjs.Option {
	return qjs.Option{
		Context:            ctx,
		CloseOnContextDone: true,
		DisableBuildCache:  recompile,
		MemoryLimit:        memory,
		MaxStackSize:       1 << 20,
		CWD:                sandboxRoot,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
	}
}

// Runner runs scripts against a fixed set of tools and globals. It is safe
// for concurrent use; each Run gets its own VM.
type Runner struct {
	tools       map[string]HostFunc
	globals     map[string]HostFunc
	opts        Options
	toolsJSON   string
	globalsJSON string
}

// NewRunner prepares a Runner. Tools whose JSName is empty use Identifier-
// style fallback of their Name (non-identifier characters become "_").
func NewRunner(tools []Tool, globals map[string]HostFunc, opts Options) *Runner {
	r := &Runner{tools: map[string]HostFunc{}, globals: globals, opts: opts.withDefaults()}
	list := make([]map[string]string, 0, len(tools))
	for _, t := range tools {
		js := t.JSName
		if js == "" {
			js = identifier(t.Name)
		}
		r.tools[t.Name] = t.Call
		list = append(list, map[string]string{"name": t.Name, "jsName": js})
	}
	names := make([]string, 0, len(globals))
	for name := range globals {
		names = append(names, name)
	}
	b, _ := json.Marshal(list)
	r.toolsJSON = string(b)
	b, _ = json.Marshal(names)
	r.globalsJSON = string(b)
	return r
}

// Run executes code as the body of an async function with the given tools
// as methods of the namespace object and globals as top-level async
// functions. It is a convenience over NewRunner(...).Run.
func Run(ctx context.Context, code string, tools []Tool, globals map[string]HostFunc, opts Options) (Output, error) {
	return NewRunner(tools, globals, opts).Run(ctx, code)
}

type hostResult struct {
	ID      int     `json:"id"`
	OK      bool    `json:"ok"`
	Payload *string `json:"payload,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// logSink collects console lines on the host side.
type logSink struct {
	mu      sync.Mutex
	lines   []string
	chars   int
	max     int
	dropped bool
	onLog   func(string)
}

func (l *logSink) add(line string) {
	l.mu.Lock()
	if l.dropped {
		l.mu.Unlock()
		return
	}
	keep := ""
	if room := l.max - l.chars; len(line) <= room {
		keep = line
	} else {
		l.dropped = true
		if room >= 256 {
			head := line[:room-96]
			keep = head + "… (" + strconv.Itoa(len(line)-len(head)) + " more characters cut)"
		}
	}
	if keep != "" {
		l.chars += len(keep)
		l.lines = append(l.lines, keep)
	}
	if l.dropped {
		l.lines = append(l.lines, "--- further console output dropped (limit "+strconv.Itoa(l.max)+" chars) ---")
	}
	l.mu.Unlock()
	if keep != "" && l.onLog != nil {
		l.onLog(keep)
	}
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	return out
}

// session is the state one Run shares between the VM goroutine and the host
// call goroutines.
type session struct {
	r       *Runner
	ctx     context.Context
	opts    Options
	results chan hostResult
	sem     chan struct{}
	logs    *logSink
	wg      sync.WaitGroup

	mu     sync.Mutex
	calls  int
	result string // JSON of the return value, "" until reported
}

// deliver hands a completed call to the driver, or gives up when the run is
// over (the channel holds every call that can be started, so this only
// blocks after a cancellation).
func (s *session) deliver(res hostResult) {
	select {
	case s.results <- res:
	case <-s.ctx.Done():
	}
}

func (s *session) hostCall(this *qjs.This) (*qjs.Value, error) {
	a := this.Args()
	if len(a) < 4 {
		return nil, errors.New("bad host call")
	}
	id, kind, name, args := int(a[0].Int32()), a[1].String(), a[2].String(), a[3].String()
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n > s.opts.MaxCalls {
		return nil, fmt.Errorf("too many host calls: limit is %d per script", s.opts.MaxCalls)
	}
	var fn HostFunc
	switch kind {
	case "tool":
		fn = s.r.tools[name]
	case "global":
		fn = s.r.globals[name]
	}
	if fn == nil {
		return nil, fmt.Errorf("unknown %s %q", kind, name)
	}
	if !json.Valid([]byte(args)) {
		return nil, errors.New("arguments are not valid JSON")
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-s.ctx.Done():
			s.deliver(hostResult{ID: id, Error: "cancelled: " + s.ctx.Err().Error()})
			return
		}
		res, err := s.call(fn, json.RawMessage(args))
		if err != nil {
			s.deliver(hostResult{ID: id, Error: err.Error()})
			return
		}
		out := string(res)
		if strings.TrimSpace(out) == "" {
			out = "null"
		}
		s.deliver(hostResult{ID: id, OK: true, Payload: &out})
	}()
	return this.Context().NewUndefined(), nil
}

// call runs a HostFunc, turning a panic into an error so one bad tool does
// not take the process down.
func (s *session) call(fn HostFunc, args json.RawMessage) (res json.RawMessage, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("tool panicked: %s", firstLine(fmt.Sprint(rec)))
		}
	}()
	return fn(s.ctx, args)
}

func (s *session) hostWait(this *qjs.This) (*qjs.Value, error) {
	select {
	case res := <-s.results:
		b, _ := json.Marshal(res)
		return this.Context().NewString(string(b)), nil
	case <-s.ctx.Done():
		return nil, errors.New("deadline exceeded while waiting for tool calls")
	}
}

func (s *session) hostLog(this *qjs.This) (*qjs.Value, error) {
	if a := this.Args(); len(a) > 0 {
		s.logs.add(a[0].String())
	}
	return this.Context().NewUndefined(), nil
}

// hostResult receives the JSON of the return value from the prelude's
// driver; the first report wins.
func (s *session) hostResult(this *qjs.This) (*qjs.Value, error) {
	if a := this.Args(); len(a) > 0 {
		s.mu.Lock()
		if s.result == "" {
			s.result = a[0].String()
		}
		s.mu.Unlock()
	}
	return this.Context().NewUndefined(), nil
}

// execute runs the prelude and the script in a fresh VM. It returns a
// *ScriptError for failures inside the script, the context's error when the
// deadline passed, or another error when the VM cannot start.
func (s *session) execute(code string) (err error) {
	rt, e := qjs.New(vmOption(s.ctx, s.opts.MemoryLimit, false))
	if e != nil {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		return fmt.Errorf("start sandbox: %w", e)
	}
	// Close panics when the module was already torn down by the deadline.
	defer func() {
		defer func() { _ = recover() }()
		rt.Close()
	}()
	defer func() {
		if rec := recover(); rec != nil {
			if s.ctx.Err() != nil {
				err = s.ctx.Err()
				return
			}
			err = &ScriptError{Message: crashMessage(fmt.Sprint(rec))}
		}
	}()
	c := rt.Context()
	c.SetFunc("__host_call", s.hostCall)
	c.SetFunc("__host_wait", s.hostWait)
	c.SetFunc("__host_log", s.hostLog)
	c.SetFunc("__host_result", s.hostResult)

	pre, e := c.Eval("codemode-prelude.js", qjs.Code(preludeSource))
	if e != nil {
		return fmt.Errorf("prelude: %w", e)
	}
	defer pre.Free()
	args := []*qjs.Value{c.NewString(s.opts.Namespace), c.NewString(s.r.toolsJSON), c.NewString(s.r.globalsJSON), c.NewString(s.opts.Hint)}
	for _, a := range args {
		defer a.Free()
	}
	v, e := c.Invoke(pre, c.Global(), args...)
	if e != nil {
		return fmt.Errorf("prelude: %w", e)
	}
	v.Free()

	// The script starts on line 1 so stack traces match the model's code.
	wrapped := "await __drive((async () => {" + code + "\n})())"
	v, e = c.Eval("script.js", qjs.Code(wrapped), qjs.FlagAsync())
	if e != nil {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		return &ScriptError{Message: cleanJSError(e.Error())}
	}
	v.Free()
	return nil
}

// Run executes code in a fresh VM.
//
// Errors: *ScriptError for failures inside the script, *TimeoutError (which
// wraps ErrTimeout) when the deadline passes, an error wrapping
// context.Canceled when ctx is cancelled, and other errors when the sandbox
// itself cannot start.
func (r *Runner) Run(ctx context.Context, code string) (Output, error) {
	Warm()
	if warmErr != nil {
		return Output{}, fmt.Errorf("start sandbox: %w", warmErr)
	}
	opts := r.opts
	start := time.Now()
	limit := opts.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < limit {
			limit = max(left, 0)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	s := &session{
		r:       r,
		ctx:     ctx,
		opts:    opts,
		results: make(chan hostResult, min(opts.MaxCalls, 1024)),
		sem:     make(chan struct{}, opts.MaxConcurrency),
		logs:    &logSink{max: opts.MaxLogChars, onLog: opts.OnLog},
	}

	// The VM runs on its own goroutine so Run can return on the deadline
	// even when the VM is blocked where wazero cannot stop it.
	done := make(chan error, 1)
	go func() { done <- s.execute(code) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		select {
		case err = <-done:
		case <-time.After(vmStopGrace):
			err = ctx.Err()
		}
	}
	elapsed := time.Since(start)

	cancel()
	waited := make(chan struct{})
	go func() { s.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(vmStopGrace):
	}

	s.mu.Lock()
	n, result := s.calls, s.result
	s.mu.Unlock()
	lines := s.logs.snapshot()

	switch e := err.(type) {
	case nil:
		if result == "" {
			result = "null"
		}
		if !json.Valid([]byte(result)) {
			return Output{}, &ScriptError{Message: "the script's return value is not valid JSON", Logs: lines, Calls: n, Elapsed: elapsed}
		}
		return Output{Result: json.RawMessage(result), Logs: lines, Calls: n, Elapsed: elapsed}, nil
	case *ScriptError:
		e.Logs, e.Calls, e.Elapsed = lines, n, elapsed
		return Output{}, e
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			return Output{}, &TimeoutError{Timeout: limit, Logs: lines, Calls: n, Elapsed: elapsed}
		}
		if errors.Is(err, context.Canceled) {
			return Output{}, fmt.Errorf("script cancelled: %w", err)
		}
		return Output{}, err
	}
}

// cleanJSError drops the prelude's frames from a stack trace.
func cleanJSError(msg string) string {
	var keep []string
	for _, line := range strings.Split(msg, "\n") {
		if strings.Contains(line, "codemode-prelude.js") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimRight(strings.Join(keep, "\n"), "\n")
}

// crashMessage describes a panic from the VM. A script that reaches the
// libc exit through import("qjs:std") ends the module with an exit code;
// anything else is reported as is, shortened.
func crashMessage(rec string) string {
	if i := strings.Index(rec, "exit_code("); i >= 0 {
		code := rec[i+len("exit_code("):]
		if j := strings.IndexByte(code, ')'); j >= 0 {
			return "the script called exit(" + code[:j] + "), which ends the sandbox without a result"
		}
	}
	return "the sandbox stopped: " + firstLine(rec)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// identifier makes a JavaScript identifier out of a name: characters that
// cannot appear in one become underscores.
func identifier(name string) string {
	var b strings.Builder
	for i, r := range name {
		ok := r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// Clip shortens s to about max bytes, keeping the head and the tail around
// a marker that states how much was cut, without splitting a UTF-8
// character. It reports whether anything was cut. Hosts use it on results
// before showing them to a model.
func Clip(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	head := max * 2 / 3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (max - head)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	marker := "\n--- TRUNCATED: " + strconv.Itoa(tail-head) + " of " + strconv.Itoa(len(s)) + " characters omitted; return less from the script or filter in code ---\n"
	return s[:head] + marker + s[tail:], true
}
