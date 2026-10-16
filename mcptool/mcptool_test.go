package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	codemode "github.com/thehumanworks/codemode-go"
	"github.com/thehumanworks/codemode-go/catalog"
)

type addIn struct {
	A int `json:"a" jsonschema:"first operand"`
	B int `json:"b" jsonschema:"second operand"`
}

type addOut struct {
	Sum int `json:"sum"`
}

type greetIn struct {
	Name  string `json:"name"`
	Style string `json:"style,omitempty" jsonschema:"greeting style"`
}

// newServer builds a server with a few tools of different shapes and the
// code tool, connects an in-memory client, and returns both.
func newServer(t *testing.T, opts Options) (*Registry, *mcp.ClientSession) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	reg := New(srv, opts)

	AddTool(reg, &mcp.Tool{Name: "app_add", Title: "Add", Description: "Add two integers."}, func(_ context.Context, _ *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, addOut, error) {
		return nil, addOut{Sum: in.A + in.B}, nil
	})

	greetSchema, _ := jsonschema.For[greetIn](nil)
	greetSchema.Properties["style"].Enum = []any{"formal", "casual"}
	greetSchema.Properties["style"].Default = json.RawMessage(`"casual"`)
	AddTool(reg, &mcp.Tool{Name: "app_greet", Description: "Greet someone by name.", InputSchema: greetSchema}, func(_ context.Context, _ *mcp.CallToolRequest, in greetIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Style + " hello " + in.Name}}}, nil, nil
	})

	AddTool(reg, &mcp.Tool{Name: "app_fail", Description: "Always fails."}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("deliberate failure")
	})

	AddTool(reg, &mcp.Tool{Name: "app_slow", Description: "Sleeps for the given milliseconds."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Ms int `json:"ms"`
	}) (*mcp.CallToolResult, any, error) {
		select {
		case <-time.After(time.Duration(in.Ms) * time.Millisecond):
			return nil, map[string]any{"slept": in.Ms}, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	})

	reg.AddTool(&mcp.Tool{Name: "app_raw", Description: "Echo arguments as a JSON text block.", InputSchema: &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"x": {Type: "integer"}},
	}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
	})

	reg.Register()
	return reg, connect(t, srv)
}

// connect attaches an in-memory client to srv.
func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// catchPanic runs f and returns what it panicked with, or "".
func catchPanic(f func()) (msg string) {
	defer func() {
		if p := recover(); p != nil {
			msg = fmt.Sprint(p)
		}
	}()
	f()
	return ""
}

func callCode(t *testing.T, cs *mcp.ClientSession, name, code string, extra map[string]any) (structured map[string]any, text string, isErr bool) {
	t.Helper()
	args := map[string]any{"code": code}
	for k, v := range extra {
		args[k] = v
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	text = res.Content[0].(*mcp.TextContent).Text
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &structured)
	return structured, text, res.IsError
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestModeOn(t *testing.T) {
	reg, cs := newServer(t, Options{Namespace: "app", StripPrefix: "app_"})
	names := toolNames(t, cs)
	if len(names) != 6 || names[len(names)-1] != "code" {
		t.Fatalf("tools: %v", names)
	}
	if got := reg.Tools(); len(got) != 5 || got[0].Name != "app_add" {
		t.Errorf("registry tools: %d", len(got))
	}

	res, _ := cs.ListTools(context.Background(), nil)
	code := res.Tools[len(res.Tools)-1]
	if !strings.Contains(code.Description, "add, greet, fail, slow, raw") || !strings.Contains(code.Description, "`app`") {
		t.Errorf("description: %s", code.Description)
	}
	if got := reg.Guidance(); !strings.Contains(got, "`code`") || !strings.Contains(got, "app.add") {
		t.Errorf("guidance: %s", got)
	}

	st, text, isErr := callCode(t, cs, "code", `
		const [sum, hi, raw] = await Promise.all([
			app.add({a: 2, b: 3}),
			app.greet({name: "ada"}),
			app.app_raw({x: 1}),
		]);
		console.log("sum is", sum.sum);
		return {sum: sum.sum, hi, raw};`, nil)
	if isErr {
		t.Fatalf("script failed: %s", text)
	}
	if st["ok"] != true || st["calls"] != float64(3) || st["truncated"] != false {
		t.Errorf("structured: %v", st)
	}
	var out struct {
		Sum int            `json:"sum"`
		Hi  string         `json:"hi"`
		Raw map[string]any `json:"raw"`
	}
	b, _ := json.Marshal(st["result"])
	_ = json.Unmarshal(b, &out)
	if out.Sum != 5 || out.Hi != "casual hello ada" || out.Raw["x"] != float64(1) {
		t.Errorf("result: %s", b)
	}
	if !strings.Contains(text, "Script completed in") || !strings.Contains(text, "<result>\n{") || !strings.Contains(text, "<console_output>\nsum is 5\n</console_output>") {
		t.Errorf("text: %s", text)
	}
	if logs, _ := st["logs"].([]any); len(logs) != 1 {
		t.Errorf("logs: %v", st["logs"])
	}
}

func TestModeOnlyAndOff(t *testing.T) {
	_, cs := newServer(t, Options{Name: "run_js", Mode: ModeOnly})
	if names := toolNames(t, cs); len(names) != 1 || names[0] != "run_js" {
		t.Fatalf("only mode tools: %v", names)
	}
	st, text, isErr := callCode(t, cs, "run_js", `return (await tools.app_add({a: 1, b: 1})).sum`, nil)
	if isErr || st["result"] != float64(2) {
		t.Errorf("only mode call: %s", text)
	}

	reg, cs2 := newServer(t, Options{Mode: ModeOff})
	if names := toolNames(t, cs2); len(names) != 5 {
		t.Errorf("off mode tools: %v", names)
	}
	if reg.Guidance() != "" {
		t.Error("off mode guidance should be empty")
	}

	if _, err := ParseMode("ONLY"); err != nil {
		t.Error(err)
	}
	if m, err := ParseMode(""); err != nil || m != ModeOn {
		t.Error("empty mode")
	}
	if _, err := ParseMode("maybe"); err == nil {
		t.Error("bad mode accepted")
	}
}

func TestValidationDefaultsAndErrors(t *testing.T) {
	_, cs := newServer(t, Options{Namespace: "app", StripPrefix: "app_"})

	_, text, isErr := callCode(t, cs, "code", `console.log("step 1"); await app.greet({name: "x", style: "shouty"})`, nil)
	if !isErr || !strings.Contains(text, "Script failed") || !strings.Contains(text, "invalid arguments") || !strings.Contains(text, "step 1") {
		t.Errorf("validation failure report: %s", text)
	}

	st, text, isErr := callCode(t, cs, "code", `try { await app.fail({}); return "no"; } catch (e) { return e.message; }`, nil)
	if isErr || st["result"] != "deliberate failure" {
		t.Errorf("catchable error: %s", text)
	}

	_, text, isErr = callCode(t, cs, "code", `await app.gree({name: "x"})`, nil)
	if !isErr || !strings.Contains(text, "Did you mean app.greet") || !strings.Contains(text, "search(") {
		t.Errorf("did-you-mean: %s", text)
	}

	_, text, isErr = callCode(t, cs, "code", `return 1`, map[string]any{"timeout": "soon"})
	if !isErr || !strings.Contains(text, "invalid timeout") {
		t.Errorf("bad timeout: %s", text)
	}
	_, text, isErr = callCode(t, cs, "code", `   `, nil)
	if !isErr || !strings.Contains(text, "code is required") {
		t.Errorf("empty code: %s", text)
	}

	st, text, isErr = callCode(t, cs, "code", `return "x".repeat(30000)`, nil)
	if isErr || st["truncated"] != true || !strings.Contains(text, "TRUNCATED") {
		t.Errorf("clipping: %v", st["truncated"])
	}
}

func TestTimeoutKeepsLogs(t *testing.T) {
	_, cs := newServer(t, Options{Namespace: "app", StripPrefix: "app_", MaxTimeout: 2 * time.Second, Sandbox: codemode.Options{Timeout: time.Second}})
	start := time.Now()
	st, text, isErr := callCode(t, cs, "code", `console.log("about to spin"); for(;;){}`, map[string]any{"timeout": "5s"})
	if !isErr || !regexp.MustCompile(`timed out after 2(\.\d+)?s \(limit 2s\)`).MatchString(text) || !strings.Contains(text, "Pass a longer timeout (max 2s)") || !strings.Contains(text, "about to spin") || st["error"] != "timeout" {
		t.Errorf("timeout report: %s", text)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
	// Numeric timeout in seconds; a tool call gets cancelled too.
	_, text, isErr = callCode(t, cs, "code", `await app.slow({ms: 10000})`, map[string]any{"timeout": 1})
	if !isErr || !strings.Contains(text, "(limit 1s)") {
		t.Errorf("numeric timeout: %s", text)
	}
}

// When the request's context has an earlier deadline than the script's
// timeout, the report says so instead of suggesting a longer timeout, and
// RunConfig.Cleanup runs.
func TestTimeoutFromRequestDeadline(t *testing.T) {
	var cleaned atomic.Int32
	_, cs := newServer(t, Options{Sandbox: codemode.Options{Timeout: time.Minute}, Prepare: func(ctx context.Context, _ map[string]any) (*RunConfig, error) {
		c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		return &RunConfig{Context: c, Cleanup: func() { cleaned.Add(1); cancel() }}, nil
	}})
	st, text, isErr := callCode(t, cs, "code", `console.log("spin"); for(;;){}`, nil)
	if !isErr || st["error"] != "timeout" || !strings.Contains(text, "the request's deadline passed before the script's 1m0s timeout") {
		t.Errorf("deadline report: %s", text)
	}
	if strings.Contains(text, "Pass a longer timeout") || strings.Contains(text, "after 1m0s") || !strings.Contains(text, "spin") {
		t.Errorf("deadline report names the wrong limit: %s", text)
	}
	if n := cleaned.Load(); n != 1 {
		t.Errorf("cleanup ran %d times", n)
	}
	// Cleanup also runs after a successful script.
	if _, text, isErr := callCode(t, cs, "code", `return 1`, nil); isErr || cleaned.Load() != 2 {
		t.Errorf("cleanup after success: %d %s", cleaned.Load(), text)
	}
}

func TestDiscoveryAndTypes(t *testing.T) {
	prepared := 0
	reg, cs := newServer(t, Options{
		Namespace:   "app",
		StripPrefix: "app_",
		TypesURI:    "test://code/types",
		Params:      map[string]*jsonschema.Schema{"region": {Type: "string", Description: "Region tools default to"}},
		Prepare: func(_ context.Context, args map[string]any) (*RunConfig, error) {
			prepared++
			region, _ := args["region"].(string)
			return &RunConfig{
				Entries: []catalog.Entry{{
					Path: "api.users.list", Kind: "api_method", Description: "List users in a region.",
					Input: map[string]any{"type": "object", "properties": map[string]any{"region": map[string]any{"type": "string"}}},
					Extra: map[string]any{"call": "await app.raw({x: 1})"},
				}},
				Warning: "api list may be stale",
				Args: func(tool string, args json.RawMessage) json.RawMessage {
					if tool != "app_greet" || region == "" {
						return args
					}
					var m map[string]any
					_ = json.Unmarshal(args, &m)
					m["name"] = m["name"].(string) + "@" + region
					b, _ := json.Marshal(m)
					return b
				},
			}, nil
		},
	})

	res, _ := cs.ListTools(context.Background(), nil)
	var code *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "code" {
			code = tool
		}
	}
	schema, _ := json.Marshal(code.InputSchema)
	if !strings.Contains(string(schema), `"region"`) || !strings.Contains(string(schema), `"required":["code"]`) {
		t.Errorf("input schema: %s", schema)
	}

	st, text, isErr := callCode(t, cs, "code", `
		const [found, member, byName, ns, group, method] = await Promise.all([
			search("add two numbers", {limit: 3}),
			describe("app.greet"),
			describe("app_greet"),
			describe("app"),
			describe("api"),
			describe("api.users.list"),
		]);
		return {
			first: found.results[0].path,
			warning: found.warning,
			memberTypes: member.types,
			memberCall: member.call,
			sameByName: byName.path === member.path,
			nsTypes: ns.types,
			groupMembers: group.members,
			methodCall: method.call,
			methodTypes: method.types,
			greeted: await app.greet({name: "ada"}),
		};`, map[string]any{"region": "eu"})
	if isErr {
		t.Fatalf("discovery script: %s", text)
	}
	var out struct {
		First        string   `json:"first"`
		Warning      string   `json:"warning"`
		MemberTypes  string   `json:"memberTypes"`
		MemberCall   string   `json:"memberCall"`
		SameByName   bool     `json:"sameByName"`
		NsTypes      string   `json:"nsTypes"`
		GroupMembers []string `json:"groupMembers"`
		MethodCall   string   `json:"methodCall"`
		MethodTypes  string   `json:"methodTypes"`
		Greeted      string   `json:"greeted"`
	}
	b, _ := json.Marshal(st["result"])
	_ = json.Unmarshal(b, &out)
	if out.First != "app.add" || out.Warning != "api list may be stale" {
		t.Errorf("search: %+v", out)
	}
	if !strings.Contains(out.MemberTypes, "greet(args: {") || !strings.Contains(out.MemberTypes, `style?: "formal" | "casual";`) || out.MemberCall != "await app.greet({...})" || !out.SameByName {
		t.Errorf("member describe: %+v", out)
	}
	if !strings.Contains(out.NsTypes, "declare const app: {") || !strings.Contains(out.NsTypes, "add(args: {") || !strings.Contains(out.NsTypes, "Promise<{\n    sum: number;\n  }>") {
		t.Errorf("namespace types: %s", out.NsTypes)
	}
	if len(out.GroupMembers) != 1 || !strings.HasPrefix(out.GroupMembers[0], "api.users.list:") {
		t.Errorf("group: %v", out.GroupMembers)
	}
	if out.MethodCall != "await app.raw({x: 1})" || !strings.Contains(out.MethodTypes, "type Args = {") {
		t.Errorf("method describe: %+v", out)
	}
	if out.Greeted != "casual hello ada@eu" {
		t.Errorf("args rewrite: %q", out.Greeted)
	}
	if prepared != 1 {
		t.Errorf("prepare ran %d times", prepared)
	}

	rr, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "test://code/types"})
	if err != nil {
		t.Fatal(err)
	}
	decl := rr.Contents[0].Text
	if !strings.Contains(decl, "declare function search(") || !strings.Contains(decl, "declare function describe(") || !strings.Contains(decl, "declare const app: {") {
		t.Errorf("types resource: %s", decl)
	}
	if reg.Declarations() != decl {
		t.Error("Declarations differs from the resource")
	}
}

func TestCallAndUnwrap(t *testing.T) {
	reg, _ := newServer(t, Options{Namespace: "app", StripPrefix: "app_"})
	for _, name := range []string{"add", "app_add", "app.add"} {
		v, err := reg.Call(context.Background(), name, json.RawMessage(`{"a": 4, "b": 5}`))
		if err != nil || string(v) != `{"sum":9}` {
			t.Errorf("%s: %s %v", name, v, err)
		}
	}
	if _, err := reg.Call(context.Background(), "greet", json.RawMessage(`{"name": "x", "style": "loud"}`)); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("validation via Call: %v", err)
	}
	if v, err := reg.Call(context.Background(), "greet", json.RawMessage(`{"name": "x"}`)); err != nil || string(v) != `"casual hello x"` {
		t.Errorf("defaults via Call: %s %v", v, err)
	}
	if _, err := reg.Call(context.Background(), "nope", nil); err == nil {
		t.Error("unknown tool")
	}

	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	cases := []struct {
		name string
		res  *mcp.CallToolResult
		err  error
		want string
	}{
		{"structured", &mcp.CallToolResult{StructuredContent: map[string]any{"a": 1}, Content: []mcp.Content{&mcp.TextContent{Text: "ignored"}}}, nil, `{"a":1}`},
		{"json object text", text(` {"b":2} `), nil, `{"b":2}`},
		{"json array text", text(`[1,2]`), nil, `[1,2]`},
		{"plain text", text("plain words"), nil, `"plain words"`},
		{"number text stays string", text("42"), nil, `"42"`},
		{"many texts", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a"}, &mcp.TextContent{Text: "b"}}}, nil, `["a","b"]`},
		{"no content", &mcp.CallToolResult{}, nil, `null`},
		{"nil result", nil, nil, `null`},
		{"non-text content", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: []byte("abc"), MIMEType: "image/png"}}}, nil, `[{"type":"image","mimeType":"image/png","data":"YWJj"}]`},
		{"null structured counts as none", &mcp.CallToolResult{StructuredContent: json.RawMessage("null"), Content: []mcp.Content{&mcp.TextContent{Text: "[3]"}}}, nil, `[3]`},
	}
	for _, tc := range cases {
		v, err := UnwrapResult(tc.res, tc.err)
		if err != nil || string(v) != tc.want {
			t.Errorf("%s: got %s %v want %s", tc.name, v, err, tc.want)
		}
	}
	r := text("bad thing")
	r.IsError = true
	if _, err := UnwrapResult(r, nil); err == nil || err.Error() != "bad thing" {
		t.Errorf("isError: %v", err)
	}
	if _, err := UnwrapResult(&mcp.CallToolResult{IsError: true}, nil); err == nil || err.Error() != "tool failed" {
		t.Errorf("isError without text: %v", err)
	}
	if _, err := UnwrapResult(text("x"), fmt.Errorf("boom")); err == nil || err.Error() != "boom" {
		t.Errorf("err passthrough: %v", err)
	}
}

func TestParseTimeout(t *testing.T) {
	def, limit := 60*time.Second, 10*time.Minute
	cases := []struct {
		in   any
		want time.Duration
		err  bool
	}{
		{nil, def, false}, {"", def, false}, {"30s", 30 * time.Second, false}, {"2m", 2 * time.Minute, false},
		{"5", 5 * time.Second, false}, {float64(1.5), 1500 * time.Millisecond, false}, {float64(0), def, false},
		{"-1s", 0, true}, {"soon", 0, true}, {true, 0, true},
		// Zero means the default, in every form.
		{"0", def, false}, {"0s", def, false}, {float64(-3), def, false},
		// NaN and infinities are errors, not 0s or overflow.
		{"NaN", 0, true}, {"Inf", 0, true}, {"-Inf", 0, true}, {"+Inf", 0, true}, {"infinity", 0, true},
		{math.NaN(), 0, true}, {math.Inf(1), 0, true}, {math.Inf(-1), 0, true},
		// Clamped to [1ns, limit] before converting.
		{"1e300", limit, false}, {float64(1e300), limit, false}, {"20m", limit, false}, {float64(3600), limit, false},
		{"1e-12", time.Nanosecond, false}, {float64(1e-10), time.Nanosecond, false},
	}
	for _, tc := range cases {
		got, err := parseTimeout(tc.in, def, limit)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%v: got %s %v", tc.in, got, err)
		}
	}
}

// Two tools that a script would reach by the same name, or a tool named
// like the code tool, panic at registration, naming both, in every mode;
// the second tool is neither recorded nor registered on the server.
func TestNameCollisionsPanic(t *testing.T) {
	h := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil }
	obj := &jsonschema.Schema{Type: "object"}
	cases := []struct {
		name          string
		opts          Options
		first, second string
		want          []string
	}{
		{"same name", Options{}, "x", "x", []string{`tool "x" added twice`}},
		{"stripped member", Options{StripPrefix: "x_"}, "x_capture", "capture", []string{`"capture"`, `"x_capture"`}},
		{"stripped member, other order", Options{StripPrefix: "x_"}, "capture", "x_capture", []string{`"capture"`, `"x_capture"`}},
		{"identifier member", Options{}, "a-b", "a_b", []string{`"a-b"`, `"a_b"`}},
		{"named like the code tool", Options{}, "", "code", []string{`tool "code"`, `the code tool "code"`}},
		{"member named like the code tool", Options{Name: "run", StripPrefix: "x_"}, "", "x_run", []string{`tool "x_run"`, `the code tool "run"`}},
	}
	for _, tc := range cases {
		for _, mode := range []Mode{ModeOn, ModeOnly, ModeOff} {
			srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
			opts := tc.opts
			opts.Mode = mode
			reg := New(srv, opts)
			if tc.first != "" {
				reg.AddTool(&mcp.Tool{Name: tc.first, InputSchema: obj}, h)
			}
			msg := catchPanic(func() { reg.AddTool(&mcp.Tool{Name: tc.second, InputSchema: obj}, h) })
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("%s/%s: panic %q lacks %q", tc.name, mode, msg, w)
				}
			}
			want := 0
			if tc.first != "" {
				want = 1
			}
			if got := len(reg.Tools()); got != want {
				t.Errorf("%s/%s: %d tools recorded", tc.name, mode, got)
			}
			if mode != ModeOnly {
				if names := toolNames(t, connect(t, srv)); len(names) != want {
					t.Errorf("%s/%s: server tools %v", tc.name, mode, names)
				}
			}
		}
	}
}

// A member whose name equals a catalogue group (api vs api.*) must not
// shadow the group: describe("api") lists the group, the full tool name
// and the namespace path reach the member.
func TestDescribeGroupVersusMember(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	reg := New(srv, Options{Namespace: "svc", StripPrefix: "svc_", Mode: ModeOnly, Prepare: func(context.Context, map[string]any) (*RunConfig, error) {
		return &RunConfig{Entries: []catalog.Entry{{Path: "api.one", Kind: "api_method", Description: "Method one."}}}, nil
	}})
	AddTool(reg, &mcp.Tool{Name: "svc_api", Description: "Call the API."}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"ok": true}, nil
	})
	reg.Register()
	cs := connect(t, srv)
	s, text, isErr := callCode(t, cs, "code", `
		const [g, byName, byPath, byNsName] = await Promise.all([describe("api"), describe("svc_api"), describe("svc.api"), describe("svc.svc_api")]);
		return [g.members.length, g.kind, byName.path, byPath.path, byNsName.path];`, nil)
	if isErr {
		t.Fatal(text)
	}
	b, _ := json.Marshal(s["result"])
	if string(b) != `[1,"namespace","svc.api","svc.api","svc.api"]` {
		t.Errorf("got %s", b)
	}
}

// A result over MaxResultChars is clipped in the structured result too, and
// error text is clipped.
func TestClipsResultsAndErrors(t *testing.T) {
	_, cs := newServer(t, Options{MaxResultChars: 100})
	st, text, isErr := callCode(t, cs, "code", `return "x".repeat(200000)`, nil)
	result, _ := st["result"].(string)
	if isErr || st["truncated"] != true || !strings.Contains(result, "TRUNCATED") || len(result) > 400 || len(text) > 1000 {
		t.Errorf("clipped result: structured %d chars, text %d chars", len(result), len(text))
	}
	if b, _ := json.Marshal(st); len(b) > 1000 {
		t.Errorf("structured content is %d bytes", len(b))
	}

	st, text, isErr = callCode(t, cs, "code", `throw new Error("e".repeat(500000))`, nil)
	msg, _ := st["error"].(string)
	if !isErr || !strings.Contains(text, "TRUNCATED") || len(text) > 1000 || len(msg) > 400 {
		t.Errorf("clipped error: text %d chars, structured %d chars", len(text), len(msg))
	}
}

// Handlers called from a script get the code tool's session; a handler that
// panics rejects the call instead of crashing the process; Registry.Call has
// no session.
func TestHandlersSeeSessionAndPanicsAreErrors(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	reg := New(srv, Options{Mode: ModeOnly})
	var seen atomic.Pointer[mcp.ServerSession]
	AddTool(reg, &mcp.Tool{Name: "whoami"}, func(_ context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		seen.Store(req.Session)
		return nil, map[string]any{"id": req.Session.ID(), "name": req.Params.Name}, nil
	})
	reg.AddTool(&mcp.Tool{Name: "raw_whoami", InputSchema: &jsonschema.Schema{Type: "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "session " + req.Session.ID()}}}, nil
	})
	reg.AddTool(&mcp.Tool{Name: "boom", InputSchema: &jsonschema.Schema{Type: "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		panic("kaboom")
	})
	reg.Register()
	cs := connect(t, srv)

	st, text, isErr := callCode(t, cs, "code", `
		const who = await tools.whoami({});
		const raw = await tools.raw_whoami();
		let caught = "";
		try { await tools.boom({}); } catch (e) { caught = e.message; }
		return {name: who.name, raw, caught};`, nil)
	if isErr {
		t.Fatalf("script: %s", text)
	}
	b, _ := json.Marshal(st["result"])
	var out struct{ Name, Raw, Caught string }
	_ = json.Unmarshal(b, &out)
	if out.Name != "whoami" || !strings.HasPrefix(out.Raw, "session") || out.Caught != `tool "boom" panicked: kaboom` {
		t.Errorf("result: %s", b)
	}
	var sessions []*mcp.ServerSession
	for ss := range srv.Sessions() {
		sessions = append(sessions, ss)
	}
	if len(sessions) != 1 || seen.Load() != sessions[0] {
		t.Errorf("handler saw session %p, server has %v", seen.Load(), sessions)
	}

	// Registry.Call carries no session: the nil dereference is an error.
	if _, err := reg.Call(context.Background(), "whoami", nil); err == nil || !strings.Contains(err.Error(), `tool "whoami" panicked`) {
		t.Errorf("Call without session: %v", err)
	}
}

// Tool names containing a dot are found by Call, Guidance and describe.
func TestDottedToolNames(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	reg := New(srv, Options{Namespace: "gh", Mode: ModeOnly})
	AddTool(reg, &mcp.Tool{Name: "gh.search", Description: "Search issues."}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Q string `json:"q"`
	}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"q": in.Q}, nil
	})
	if g := reg.Guidance(); !strings.Contains(g, "gh.gh_search(") {
		t.Errorf("guidance: %s", g)
	}
	for _, name := range []string{"gh.search", "gh_search", "gh.gh_search"} {
		if v, err := reg.Call(context.Background(), name, json.RawMessage(`{"q":"x"}`)); err != nil || string(v) != `{"q":"x"}` {
			t.Errorf("Call(%q): %s %v", name, v, err)
		}
	}
	reg.Register()
	st, text, isErr := callCode(t, connect(t, srv), "code", `
		const d = await describe("gh.search");
		return [d.path, (await gh["gh.search"]({q: "y"})).q, (await gh.gh_search({q: "z"})).q];`, nil)
	b, _ := json.Marshal(st["result"])
	if isErr || string(b) != `["gh.gh_search","y","z"]` {
		t.Errorf("script: %s %s", b, text)
	}
}

// A tool added after Register shows up in the code tool's description.
func TestAddToolAfterRegister(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	reg := New(srv, Options{Namespace: "app", StripPrefix: "app_"})
	AddTool(reg, &mcp.Tool{Name: "app_add", Description: "Add."}, func(_ context.Context, _ *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, addOut, error) {
		return nil, addOut{Sum: in.A + in.B}, nil
	})
	reg.Register()
	cs := connect(t, srv)
	AddTool(reg, &mcp.Tool{Name: "app_late", Description: "Added late."}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"late": true}, nil
	})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Name == "code" && !strings.Contains(tool.Description, "add, late") {
			t.Errorf("code description not refreshed: %s", tool.Description)
		}
	}
	if len(names) != 3 {
		t.Errorf("tools: %v", names)
	}
	if st, text, isErr := callCode(t, cs, "code", `return (await app.late()).late`, nil); isErr || st["result"] != true {
		t.Errorf("late tool: %s", text)
	}
}

// Tools the SDK rejects panic in every mode, with the SDK's message, before
// anything is recorded; schemas the SDK accepts never panic.
func TestToolChecksMatchSDK(t *testing.T) {
	h := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil }
	for _, mode := range []Mode{ModeOn, ModeOnly, ModeOff} {
		reg := New(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), Options{Mode: mode})
		msg := catchPanic(func() {
			AddTool(reg, &mcp.Tool{Name: "str"}, func(context.Context, *mcp.CallToolRequest, string) (*mcp.CallToolResult, any, error) {
				return nil, nil, nil
			})
		})
		if !strings.Contains(msg, `input schema must have type "object"`) {
			t.Errorf("%s: In=string: %q", mode, msg)
		}
		if msg := catchPanic(func() { reg.AddTool(&mcp.Tool{Name: "noschema"}, h) }); !strings.Contains(msg, "missing input schema") {
			t.Errorf("%s: nil input schema: %q", mode, msg)
		}
		if n := len(reg.Tools()); n != 0 {
			t.Errorf("%s: %d rejected tools recorded", mode, n)
		}

		// Accepted by (*mcp.Server).AddTool, so accepted here: an invalid
		// default and a remote $ref.
		msg = catchPanic(func() {
			reg.AddTool(&mcp.Tool{Name: "bad_default", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer", "default": "seven"}}}}, h)
			reg.AddTool(&mcp.Tool{Name: "remote_ref", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"$ref": "https://example.com/x.json"}}}}, h)
			_ = reg.Declarations()
		})
		if msg != "" || len(reg.Tools()) != 2 {
			t.Errorf("%s: schemas the SDK accepts: %q", mode, msg)
		}
	}
}

// The registry keeps its own copy of a tool and leaves the caller's alone.
func TestToolIsCopied(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	reg := New(srv, Options{Mode: ModeOn})
	typed := &mcp.Tool{Name: "typed", Description: "before"}
	AddTool(reg, typed, func(_ context.Context, _ *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, addOut, error) {
		return nil, addOut{Sum: in.A + in.B}, nil
	})
	raw := &mcp.Tool{Name: "raw", Description: "before", InputSchema: &jsonschema.Schema{Type: "object"}}
	reg.AddTool(raw, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })
	if typed.InputSchema != nil || typed.OutputSchema != nil {
		t.Error("AddTool wrote schemas into the caller's tool")
	}
	typed.Description, raw.Description = "after", "after"
	for _, tool := range reg.Tools() {
		if tool.Description != "before" || tool.InputSchema == nil {
			t.Errorf("recorded %s changed: %q", tool.Name, tool.Description)
		}
	}
	if d, err := reg.Catalog().Describe("tools.typed"); err != nil || d["description"] != "before" {
		t.Errorf("catalogue: %v %v", d, err)
	}
	res, err := connect(t, srv).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Description != "before" {
			t.Errorf("server %s: %q", tool.Name, tool.Description)
		}
	}
}

// Params values are validated before Prepare; the default timeout is
// clamped to the maximum.
func TestParamsAndTimeoutDefaults(t *testing.T) {
	var prepared atomic.Int32
	reg, cs := newServer(t, Options{
		MaxTimeout: 30 * time.Second,
		Sandbox:    codemode.Options{Timeout: 2 * time.Minute},
		Params:     map[string]*jsonschema.Schema{"region": {Type: "string", Enum: []any{"eu", "us"}}},
		Prepare: func(context.Context, map[string]any) (*RunConfig, error) {
			prepared.Add(1)
			return nil, nil
		},
	})
	_, text, isErr := callCode(t, cs, "code", `return 1`, map[string]any{"region": "mars"})
	if !isErr || !strings.Contains(text, "invalid arguments: region") || prepared.Load() != 0 {
		t.Errorf("invalid param: %s (prepared %d)", text, prepared.Load())
	}
	if st, text, isErr := callCode(t, cs, "code", `return 1`, map[string]any{"region": "eu"}); isErr || st["result"] != float64(1) || prepared.Load() != 1 {
		t.Errorf("valid param: %s", text)
	}
	if reg.opts.Sandbox.Timeout != 30*time.Second {
		t.Errorf("default timeout %s", reg.opts.Sandbox.Timeout)
	}
	if d := reg.inputSchema().Properties["timeout"].Description; !strings.Contains(d, "Default 30s, max 30s") {
		t.Errorf("timeout description: %s", d)
	}
	if msg := catchPanic(func() {
		New(nil, Options{Params: map[string]*jsonschema.Schema{"bad": {Ref: "https://example.com/x.json"}}})
	}); !strings.Contains(msg, `param "bad"`) {
		t.Errorf("unresolvable param schema: %q", msg)
	}
}

// A handler that panics during a direct client call yields an error result
// with the same text as a script call, and the server keeps serving.
func TestWirePanicsAreErrorResults(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	reg := New(srv, Options{})
	AddTool(reg, &mcp.Tool{Name: "typed_boom"}, func(context.Context, *mcp.CallToolRequest, struct {
		N int `json:"n,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		panic("typed kaboom")
	})
	reg.AddTool(&mcp.Tool{Name: "raw_boom", InputSchema: &jsonschema.Schema{Type: "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var m map[string]int
		m["x"]++ // nil map write
		return nil, nil
	})
	AddTool(reg, &mcp.Tool{Name: "ok"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"ok": true}, nil
	})
	reg.Register()
	cs := connect(t, srv)

	for name, want := range map[string]string{
		"typed_boom": `tool "typed_boom" panicked: typed kaboom`,
		"raw_boom":   `tool "raw_boom" panicked: assignment to entry in nil map`,
	} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("%s: transport error %v", name, err)
		}
		if !res.IsError || len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != want {
			b, _ := json.Marshal(res)
			t.Errorf("%s: %s", name, b)
		}
		// Through a script the same text comes back as the rejection.
		st, text, isErr := callCode(t, cs, "code", `try { await tools.`+name+`({}); return "no error"; } catch (e) { return e.message; }`, nil)
		if isErr || st["result"] != want {
			t.Errorf("%s via script: %v %s", name, st["result"], text)
		}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ok", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("server after panics: %v %v", err, res)
	}
}
