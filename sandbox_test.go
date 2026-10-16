package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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
}
