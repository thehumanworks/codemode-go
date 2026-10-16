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
//   - plain ECMAScript: no network, filesystem, timers, imports, or modules.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fastschema/qjs"
)

//go:embed prelude.js
var preludeSource string

// HostFunc handles one call from a script. args is the JSON the script
// passed: an object for tools, an array of positional arguments for
// globals. The returned JSON becomes the resolved value ("null" when
// empty); an error rejects the promise with the error's message.
//
// The context is cancelled when the script finishes, fails, or times out,
// so a HostFunc that blocks must honour it.
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
	// MaxLogChars caps captured console output. Default 64 KiB.
	MaxLogChars int
	// Hint is appended to the error a script gets for an unknown namespace
	// member, after the did-you-mean suggestions. Hosts that provide
	// discovery globals say so here (catalog.Hint builds one).
	Hint string
	// OnLog receives each console line as the script prints it, before any
	// cap is applied. It runs on the script's goroutine, so it must return
	// quickly. Optional.
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
// error, a stall, running out of memory, or exceeding the call limit. Logs
// hold what the script printed before failing.
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
	Timeout time.Duration
	Logs    []string
	Calls   int
	Elapsed time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("script timed out after %s; tool calls still running were cancelled", e.Timeout)
}

// Unwrap makes errors.Is(err, ErrTimeout) true.
func (e *TimeoutError) Unwrap() error { return ErrTimeout }

var warmOnce sync.Once

// Warm compiles the QuickJS module so the first script does not pay for it
// (about 300ms). It is safe to call from several goroutines and more than
// once; typically hosts call it in a goroutine at start-up.
//
// qjs compiles the module once per process and bakes CloseOnContextDone into
// that compilation, so every qjs.New in this package passes the flag: a
// runtime created without it would silently disable the hard timeout for all
// later scripts.
func Warm() {
	warmOnce.Do(func() {
		rt, err := qjs.New(qjs.Option{Context: context.Background(), CloseOnContextDone: true, MemoryLimit: 16 << 20})
		if err == nil {
			rt.Close()
		}
	})
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
	if l.onLog != nil {
		l.onLog(line)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.chars+len(line) > l.max {
		if !l.dropped {
			l.dropped = true
			l.lines = append(l.lines, "--- further console output dropped (limit "+strconv.Itoa(l.max)+" chars) ---")
		}
		return
	}
	l.chars += len(line)
	l.lines = append(l.lines, line)
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	return out
}

// Run executes code in a fresh VM.
//
// Errors: *ScriptError for failures inside the script, *TimeoutError (which
// wraps ErrTimeout) when the deadline passes, an error wrapping
// context.Canceled when ctx is cancelled, and other errors when the sandbox
// itself cannot start.
func (r *Runner) Run(ctx context.Context, code string) (out Output, err error) {
	opts := r.opts
	start := time.Now()
	limit := opts.Timeout
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < limit {
		limit = time.Until(dl)
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	rt, err := qjs.New(qjs.Option{
		Context:            ctx,
		CloseOnContextDone: true,
		MemoryLimit:        opts.MemoryLimit,
		MaxStackSize:       1 << 20,
	})
	if err != nil {
		return out, fmt.Errorf("start sandbox: %w", err)
	}
	// Close panics when the module was already torn down by the deadline.
	defer func() {
		defer func() { _ = recover() }()
		rt.Close()
	}()
	c := rt.Context()

	results := make(chan hostResult, 256)
	sem := make(chan struct{}, opts.MaxConcurrency)
	logs := &logSink{max: opts.MaxLogChars, onLog: opts.OnLog}
	var wg sync.WaitGroup
	var mu sync.Mutex
	calls := 0

	c.SetFunc("__host_call", func(this *qjs.This) (*qjs.Value, error) {
		a := this.Args()
		if len(a) < 4 {
			return nil, fmt.Errorf("bad host call")
		}
		id, kind, name, args := int(a[0].Int32()), a[1].String(), a[2].String(), a[3].String()
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > opts.MaxCalls {
			results <- hostResult{ID: id, Error: fmt.Sprintf("too many host calls: limit is %d per script", opts.MaxCalls)}
			return this.Context().NewUndefined(), nil
		}
		var fn HostFunc
		switch kind {
		case "tool":
			fn = r.tools[name]
		case "global":
			fn = r.globals[name]
		}
		if fn == nil {
			results <- hostResult{ID: id, Error: fmt.Sprintf("unknown %s %q", kind, name)}
			return this.Context().NewUndefined(), nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- hostResult{ID: id, Error: "cancelled: " + ctx.Err().Error()}
				return
			}
			res, err := fn(ctx, json.RawMessage(args))
			if err != nil {
				results <- hostResult{ID: id, Error: err.Error()}
				return
			}
			s := string(res)
			if strings.TrimSpace(s) == "" {
				s = "null"
			}
			results <- hostResult{ID: id, OK: true, Payload: &s}
		}()
		return this.Context().NewUndefined(), nil
	})
	c.SetFunc("__host_wait", func(this *qjs.This) (*qjs.Value, error) {
		select {
		case res := <-results:
			b, _ := json.Marshal(res)
			return this.Context().NewString(string(b)), nil
		case <-ctx.Done():
			return nil, fmt.Errorf("deadline exceeded while waiting for tool calls")
		}
	})
	c.SetFunc("__host_log", func(this *qjs.This) (*qjs.Value, error) {
		if a := this.Args(); len(a) > 0 {
			logs.add(a[0].String())
		}
		return this.Context().NewUndefined(), nil
	})

	var resultJSON string
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				if ctx.Err() != nil {
					err = ctx.Err()
				} else {
					err = fmt.Errorf("sandbox crashed: %v", rec)
				}
			}
		}()
		pre, e := c.Eval("codemode-prelude.js", qjs.Code(preludeSource))
		if e != nil {
			err = fmt.Errorf("prelude: %w", e)
			return
		}
		defer pre.Free()
		args := []*qjs.Value{c.NewString(opts.Namespace), c.NewString(r.toolsJSON), c.NewString(r.globalsJSON), c.NewString(opts.Hint)}
		for _, a := range args {
			defer a.Free()
		}
		v, e := c.Invoke(pre, c.Global(), args...)
		if e != nil {
			err = fmt.Errorf("prelude: %w", e)
			return
		}
		v.Free()

		wrapped := "await __drive((async () => {\n" + code + "\n})())"
		v, e = c.Eval("script.js", qjs.Code(wrapped), qjs.FlagAsync())
		if e != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
				return
			}
			err = &ScriptError{Message: cleanJSError(e.Error())}
			return
		}
		v.Free()
		// The prelude stores the JSON of the return value on a global; a
		// value read straight from the eval result can be freed too early.
		rv := c.Global().GetPropertyStr("__result")
		defer rv.Free()
		resultJSON = "null"
		if rv.IsString() {
			if s := rv.String(); s != "" {
				resultJSON = s
			}
		}
	}()

	elapsed := time.Since(start)
	mu.Lock()
	n := calls
	mu.Unlock()
	cancel()
	wg.Wait()
	lines := logs.snapshot()

	switch e := err.(type) {
	case nil:
		return Output{Result: json.RawMessage(resultJSON), Logs: lines, Calls: n, Elapsed: elapsed}, nil
	case *ScriptError:
		e.Logs, e.Calls, e.Elapsed = lines, n, elapsed
		return out, e
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			return out, &TimeoutError{Timeout: limit, Logs: lines, Calls: n, Elapsed: elapsed}
		}
		if errors.Is(err, context.Canceled) {
			return out, fmt.Errorf("script cancelled: %w", err)
		}
		return out, err
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

// Clip shortens s to about max characters, keeping the head and the tail
// around a marker that states how much was cut. It reports whether anything
// was cut. Hosts use it on results before showing them to a model.
func Clip(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	head := max * 2 / 3
	tail := max - head
	marker := "\n--- TRUNCATED: " + strconv.Itoa(len(s)-max) + " of " + strconv.Itoa(len(s)) + " characters omitted; return less from the script or filter in code ---\n"
	return s[:head] + marker + s[len(s)-tail:], true
}
