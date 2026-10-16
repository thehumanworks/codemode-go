package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fastschema/qjs"
)

func testTools() []Tool {
	return []Tool{
		{Name: "t_sleep", JSName: "sleep", Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var in struct{ Ms int }
			_ = json.Unmarshal(args, &in)
			select {
			case <-time.After(time.Duration(in.Ms) * time.Millisecond):
				return json.RawMessage(fmt.Sprintf(`{"slept":%d}`, in.Ms)), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}},
		{Name: "t_echo", JSName: "echo", Call: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) { return args, nil }},
		{Name: "t_fail", JSName: "fail", Call: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) { return nil, errors.New("nope") }},
		{Name: "t_text", JSName: "text", Call: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"plain"`), nil
		}},
	}
}

func testGlobals() map[string]HostFunc {
	return map[string]HostFunc{
		"search": func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"results":[{"path":"rex.echo"}],"args":` + string(args) + `}`), nil
		},
	}
}

func run(t *testing.T, code string, opts Options) (Output, error) {
	t.Helper()
	opts.Namespace = "rex"
	return Run(context.Background(), code, testTools(), testGlobals(), opts)
}

func TestRunParallelAndSequential(t *testing.T) {
	Warm()
	out, err := run(t, `
		const t0 = Date.now();
		const r = await Promise.all([rex.sleep({Ms: 200}), rex.sleep({Ms: 200}), rex.t_sleep({Ms: 200})]);
		const e = await rex.echo({a: 1});
		console.log("done", r.length, e);
		return {ms: Date.now() - t0, e, n: r.length};`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Ms int
		E  map[string]any
		N  int
	}
	_ = json.Unmarshal(out.Result, &v)
	if v.N != 3 || v.Ms > 450 || v.E["a"] != float64(1) {
		t.Errorf("unexpected result %s", out.Result)
	}
	if len(out.Logs) != 1 || !strings.HasPrefix(out.Logs[0], "done 3") || out.Calls != 4 {
		t.Errorf("logs=%v calls=%d", out.Logs, out.Calls)
	}
}

func TestRunReturnShapes(t *testing.T) {
	cases := map[string]string{
		`try { await null; return "ok"; } catch (e) { return "c"; }`:                         `"ok"`,
		`try { await rex.echo({a: 1}); return "ok"; } catch (e) { return "c"; }`:             `"ok"`,
		`try { await rex.echo({a: 1}); } catch (e) { return "c"; } return "after";`:          `"after"`,
		`try { await rex.fail({}); return "no"; } catch (e) { return {caught: e.message}; }`: `{"caught":"nope"}`,
		`return 42`:                          `42`,
		`return [1, "two", null, {x: true}]`: `[1,"two",null,{"x":true}]`,
		`return undefined`:                   `null`,
		`return () => 1`:                     `null`,
		`const s = "x".repeat(100000); return s.length`: `100000`,
	}
	for code, want := range cases {
		out, err := run(t, code, Options{})
		if err != nil {
			t.Errorf("%s: %v", code, err)
			continue
		}
		if string(out.Result) != want {
			t.Errorf("%s: got %s want %s", code, out.Result, want)
		}
	}
	_, err := run(t, `const a = {}; a.self = a; return a`, Options{})
	if err == nil || !strings.Contains(err.Error(), "serialised") {
		t.Errorf("circular: %v", err)
	}
}

func TestRunLogsStable(t *testing.T) {
	for i := 0; i < 30; i++ {
		out, err := run(t, `console.log("a", 1, {b: 2}); console.warn("second line"); console.error(new Error("e")); return "r"`, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Logs) != 3 || out.Logs[0] != `a 1 {"b":2}` || out.Logs[1] != "second line" || !strings.HasPrefix(out.Logs[2], "Error: e") || string(out.Result) != `"r"` {
			t.Fatalf("iteration %d: logs=%q result=%s", i, out.Logs, out.Result)
		}
	}
}

func TestRunOnLog(t *testing.T) {
	var mu sync.Mutex
	var live []string
	_, err := run(t, `console.log("one"); await rex.echo({}); console.log("two");`, Options{OnLog: func(line string) {
		mu.Lock()
		live = append(live, line)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(live, ",") != "one,two" {
		t.Errorf("live logs: %v", live)
	}
}

func TestRunGlobalsAndText(t *testing.T) {
	out, err := run(t, `const s = await search("echo", {limit: 2}); const p = await rex.text(); return [s.results[0].path, s.args, p];`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Result) != `["rex.echo",["echo",{"limit":2}],"plain"]` {
		t.Errorf("got %s", out.Result)
	}
}

func TestRunErrors(t *testing.T) {
	cases := []struct {
		name, code, want string
	}{
		{"throw", `console.log("before"); throw new Error("boom")`, "Error: boom"},
		{"reject", `await rex.fail({})`, "nope"},
		{"caught", `try { await rex.fail({}) } catch (e) { throw new Error("caught " + e.message) }`, "caught nope"},
		{"unknown member", `await rex.capture({})`, "rex.capture does not exist"},
		{"unknown member hint", `await rex.ech({})`, "Did you mean rex.echo? Try search()."},
		{"stalled", `await new Promise(() => {})`, "stalled"},
		{"syntax", `const = 1`, "SyntaxError"},
		{"oom", `let s = "x"; for (let i = 0; i < 40; i++) s += s; return s.length`, "out of memory"},
		{"no return", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(t, tc.code, Options{MemoryLimit: 16 << 20, Hint: "Try search()."})
			if tc.want == "" {
				if err != nil || string(out.Result) != "null" {
					t.Fatalf("err=%v out=%s", err, out.Result)
				}
				return
			}
			var se *ScriptError
			if !errors.As(err, &se) {
				t.Fatalf("want ScriptError, got %v", err)
			}
			if !strings.Contains(se.Message, tc.want) {
				t.Errorf("want %q in %q", tc.want, se.Message)
			}
			if tc.name == "throw" && (len(se.Logs) != 1 || se.Logs[0] != "before") {
				t.Errorf("logs lost: %v", se.Logs)
			}
			if strings.Contains(se.Message, "prelude") {
				t.Errorf("prelude frames leaked: %q", se.Message)
			}
		})
	}
}

func TestRunTimeouts(t *testing.T) {
	for _, code := range []string{`console.log("started"); for(;;){}`, `console.log("started"); return await rex.sleep({Ms: 5000})`} {
		start := time.Now()
		_, err := run(t, code, Options{Timeout: 500 * time.Millisecond})
		var te *TimeoutError
		if !errors.Is(err, ErrTimeout) || !errors.As(err, &te) {
			t.Errorf("%s: want timeout, got %v", code, err)
			continue
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("%s: took %s", code, time.Since(start))
		}
		if len(te.Logs) != 1 || te.Logs[0] != "started" || te.Timeout != 500*time.Millisecond || te.Elapsed < 400*time.Millisecond {
			t.Errorf("%s: timeout error %+v", code, te)
		}
	}
	// A shorter context deadline wins over Options.Timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, `for(;;){}`, testTools(), nil, Options{Timeout: 10 * time.Second})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("ctx deadline: %v", err)
	}
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := Run(ctx, `return await rex.sleep({Ms: 5000})`, testTools(), nil, Options{Namespace: "rex"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Errorf("want cancellation, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("cancel took %s", time.Since(start))
	}
}

func TestRunLimits(t *testing.T) {
	_, err := run(t, `await Promise.all(Array.from({length: 5}, () => rex.echo({})))`, Options{MaxCalls: 3})
	if err == nil || !strings.Contains(err.Error(), "too many host calls") {
		t.Errorf("want call limit, got %v", err)
	}
	out, err := run(t, `for (let i = 0; i < 100; i++) console.log("x".repeat(50)); return 1`, Options{MaxLogChars: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Logs) != 11 || !strings.Contains(out.Logs[10], "dropped") {
		t.Errorf("log cap: %d lines: %q", len(out.Logs), out.Logs)
	}
	// Concurrency cap: 4 sleeps of 200ms with MaxConcurrency 2 take two rounds.
	start := time.Now()
	_, err = run(t, `await Promise.all([1,2,3,4].map(() => rex.sleep({Ms: 200})))`, Options{MaxConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 380*time.Millisecond {
		t.Errorf("concurrency cap not applied: %s", d)
	}
}

func TestHostBridgeHidden(t *testing.T) {
	out, err := run(t, `return [typeof __host_call, typeof __host_wait, typeof __host_log, typeof __drive, typeof rex, typeof search]`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Result) != `["undefined","undefined","undefined","function","object","function"]` {
		t.Errorf("got %s", out.Result)
	}
}

func TestRunnerReuseAndJSNameFallback(t *testing.T) {
	r := NewRunner([]Tool{{Name: "my-tool", Call: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"ok"`), nil
	}}}, nil, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := r.Run(context.Background(), `return [await tools.my_tool({}), await tools["my-tool"]({})]`)
			if err != nil || string(out.Result) != `["ok","ok"]` {
				t.Errorf("runner: %s %v", out.Result, err)
			}
		}()
	}
	wg.Wait()
}

func TestClip(t *testing.T) {
	s := strings.Repeat("a", 500) + strings.Repeat("z", 500)
	out, cut := Clip(s, 300)
	if !cut || !strings.HasPrefix(out, "aaaa") || !strings.HasSuffix(out, "zzzz") || !strings.Contains(out, "TRUNCATED: 700 of 1000") {
		t.Errorf("clip: %q", out[:60]+"..."+out[len(out)-20:])
	}
	if o, c := Clip("short", 300); c || o != "short" {
		t.Error("no clip expected")
	}
	// Multi-byte characters are never split, so the result marshals cleanly.
	u := strings.Repeat("é", 400) + strings.Repeat("日", 400)
	out, cut = Clip(u, 301)
	if !cut || !utf8.ValidString(out) || !strings.HasPrefix(out, "éé") || !strings.HasSuffix(out, "日日") {
		t.Errorf("utf8 clip: valid=%v %q", utf8.ValidString(out), out[:12])
	}
}

// shortGrace makes the deadline watchdog return quickly for tests that block
// the VM where wazero cannot stop it.
func shortGrace(t *testing.T) {
	t.Helper()
	old := vmStopGrace
	vmStopGrace = 200 * time.Millisecond
	t.Cleanup(func() { vmStopGrace = old })
}

func TestSandboxIsolation(t *testing.T) {
	// The quickjs-libc helpers and qjs's own class are gone from globalThis.
	out, err := run(t, `return Object.getOwnPropertyNames(globalThis).filter(n => /QJS|__host|^std$|^os$|^print$|Timeout|Interval|scriptArgs|bjson|^gc$/.test(n)).concat([typeof setTimeout, typeof print, typeof std])`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Result) != `["undefined","undefined","undefined"]` {
		t.Errorf("libc globals still visible: %s", out.Result)
	}
	// The embedded build has no module loader and no libc modules, so a
	// dynamic import of anything fails.
	out, err = run(t, `
		const out = {};
		for (const m of ["qjs:os", "qjs:std", "qjs:bjson", "./x.js", "fs"]) {
			try { await import(m); out[m] = "loaded"; } catch (e) { out[m] = e.constructor.name + ": " + e.message; }
		}
		return out;`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Result, &got); err != nil || len(got) != 5 {
		t.Fatalf("imports: %s %v", out.Result, err)
	}
	for m, msg := range got {
		// QuickJS normalises the specifier ("./x.js" becomes "x.js"), so
		// only the error class and the loader's wording are checked.
		if !strings.HasPrefix(msg, "ReferenceError: could not load module '") || !strings.HasSuffix(msg, "': this sandbox has no module loader") {
			t.Errorf("import(%q): %s", m, msg)
		}
	}
}

func TestWasmHasNoLibc(t *testing.T) {
	// Before the prelude runs, the raw VM must already lack every
	// quickjs-libc name; the prelude's deletions are defence in depth, not
	// the isolation boundary. (gc and navigator are qjs's own harmless
	// globals, and the prelude removes gc.)
	Warm()
	if warmErr != nil {
		t.Fatal(warmErr)
	}
	rt, err := qjs.New(vmOption(context.Background(), 16<<20, false))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var seen string
	rt.Context().SetFunc("__report", func(this *qjs.This) (*qjs.Value, error) {
		seen = this.Args()[0].String()
		return this.Context().NewUndefined(), nil
	})
	_, err = rt.Context().Eval("probe.js", qjs.Code(`__report(JSON.stringify(["std","os","bjson","print","setTimeout","setInterval","clearTimeout","clearInterval","scriptArgs"].filter(n => n in globalThis)))`))
	if err != nil {
		t.Fatal(err)
	}
	if seen != `[]` {
		t.Errorf("libc globals present in the raw VM: %s", seen)
	}
}

func TestRunBusyLoopStopsOnTime(t *testing.T) {
	shortGrace(t)
	// Without libc there is nothing that can block inside the VM; a busy
	// loop is interrupted at the deadline, well inside the grace period.
	start := time.Now()
	_, err := run(t, `console.log("before"); for (;;) {} return "never"`, Options{Timeout: 150 * time.Millisecond})
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("want timeout, got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Run took %s", d)
	}
	if len(te.Logs) != 1 || te.Logs[0] != "before" || te.Timeout != 150*time.Millisecond {
		t.Errorf("timeout error: %+v", te)
	}
}

func TestRunToolPanic(t *testing.T) {
	// A HostFunc that panics rejects the call instead of ending the process.
	tools := append(testTools(), Tool{Name: "boom", Call: func(context.Context, json.RawMessage) (json.RawMessage, error) { panic("kaboom") }})
	out, err := Run(context.Background(), `try { await app.boom({}); } catch (e) { return e.message; }`, tools, nil, Options{Namespace: "app"})
	if err != nil || string(out.Result) != `"tool panicked: kaboom"` {
		t.Errorf("panic: %s %v", out.Result, err)
	}
}

func TestRunCallBursts(t *testing.T) {
	// Calls over the limit fail synchronously, so neither a fire-and-forget
	// burst nor an awaited one can wedge the result channel.
	out, err := run(t, `for (let i = 0; i < 600; i++) rex.echo({i}).catch(() => {}); return "done"`, Options{})
	if err != nil || string(out.Result) != `"done"` || out.Calls != 600 {
		t.Errorf("fire and forget: %s %d %v", out.Result, out.Calls, err)
	}
	out, err = run(t, `try { await Promise.all(Array.from({length: 600}, (_, i) => rex.echo({i}))); } catch (e) { return e.message; }`, Options{})
	if err != nil || string(out.Result) != `"too many host calls: limit is 200 per script"` {
		t.Errorf("awaited: %s %v", out.Result, err)
	}
	// Unknown names reported by the host are catchable too.
	out, err = run(t, `try { await rex.t_echo.call(null, {}); return "called"; } catch (e) { return e.message; }`, Options{})
	if err != nil || string(out.Result) != `"called"` {
		t.Errorf("full name call: %s %v", out.Result, err)
	}
}

func TestRunResultIntegrity(t *testing.T) {
	out, err := run(t, `globalThis.__result = "{not json"; return {host: typeof __host_result, drive: typeof __drive}`, Options{})
	if err != nil || string(out.Result) != `{"host":"undefined","drive":"function"}` {
		t.Errorf("forged global: %s %v", out.Result, err)
	}
	// The driver cannot be re-entered by the script to report another value.
	_, err = run(t, `await __drive(Promise.resolve(2)); return 1`, Options{})
	var se *ScriptError
	if !errors.As(err, &se) || !strings.Contains(se.Message, "__drive is not available") {
		t.Errorf("second drive: %v", err)
	}
	// Function.prototype.call and apply are pinned.
	out, err = run(t, `const c = Function.prototype.call; try { Function.prototype.call = () => 1; } catch {} return Function.prototype.call === c && Object.getOwnPropertyDescriptor(Function.prototype, "apply").writable === false`, Options{})
	if err != nil || string(out.Result) != "true" {
		t.Errorf("pinned call: %s %v", out.Result, err)
	}
}

func TestRunStackLineNumbers(t *testing.T) {
	_, err := run(t, "const a = 1;\nconst b = 2;\nthrow new Error('line three')", Options{})
	var se *ScriptError
	if !errors.As(err, &se) || !strings.Contains(se.Message, "script.js:3:") || strings.Contains(se.Message, "prelude") {
		t.Errorf("stack: %v", err)
	}
}

func TestRunExpiredContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	_, err := Run(ctx, `return 1`, testTools(), nil, Options{})
	var te *TimeoutError
	if !errors.As(err, &te) || te.Timeout != 0 {
		t.Errorf("expired context: %v", err)
	}
	if strings.Contains(err.Error(), "cancelled") {
		t.Errorf("message mentions cancelled calls with none started: %v", err)
	}
}

func TestLogCapDropsEverythingAfterMarker(t *testing.T) {
	var seen []string
	out, err := run(t, `console.log("x".repeat(5000)); console.log("after1"); console.log("after2"); return 1`, Options{MaxLogChars: 1000, OnLog: func(l string) { seen = append(seen, l) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Logs) != 2 || len(out.Logs[0]) > 1000 || !strings.Contains(out.Logs[0], "more characters cut") || !strings.Contains(out.Logs[1], "dropped") {
		t.Errorf("oversized line: %d lines, first %d chars", len(out.Logs), len(out.Logs[0]))
	}
	if len(seen) != 1 || seen[0] != out.Logs[0] {
		t.Errorf("OnLog saw %d lines, want only the kept one", len(seen))
	}
	// Too little room left: the oversized line is dropped whole.
	out, err = run(t, `console.log("ok"); console.log("x".repeat(5000)); console.log("after"); return 1`, Options{MaxLogChars: 100})
	if err != nil || len(out.Logs) != 2 || out.Logs[0] != "ok" || !strings.Contains(out.Logs[1], "dropped") {
		t.Errorf("small cap: %q %v", out.Logs, err)
	}
}

func TestWarmSelfTest(t *testing.T) {
	Warm()
	if warmErr != nil {
		t.Fatal(warmErr)
	}
	fi, err := os.Stat(sandboxRoot)
	if err != nil || !fi.IsDir() {
		t.Fatalf("sandbox root: %v", err)
	}
	if entries, _ := os.ReadDir(sandboxRoot); len(entries) != 0 {
		t.Errorf("sandbox root is not empty: %d entries", len(entries))
	}
}
