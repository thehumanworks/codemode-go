package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	return reg, cs
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
	if !isErr || !strings.Contains(text, "timed out after 2s") || !strings.Contains(text, "about to spin") || st["error"] != "timeout" {
		t.Errorf("timeout report: %s", text)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
	// Numeric timeout in seconds; a tool call gets cancelled too.
	_, text, isErr = callCode(t, cs, "code", `await app.slow({ms: 10000})`, map[string]any{"timeout": 1})
	if !isErr || !strings.Contains(text, "timed out after 1s") {
		t.Errorf("numeric timeout: %s", text)
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
	def := 60 * time.Second
	cases := []struct {
		in   any
		want time.Duration
		err  bool
	}{
		{nil, def, false}, {"", def, false}, {"30s", 30 * time.Second, false}, {"2m", 2 * time.Minute, false},
		{"5", 5 * time.Second, false}, {float64(1.5), 1500 * time.Millisecond, false}, {float64(0), def, false},
		{"-1s", 0, true}, {"soon", 0, true}, {true, 0, true},
	}
	for _, tc := range cases {
		got, err := parseTimeout(tc.in, def)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%v: got %s %v", tc.in, got, err)
		}
	}
}

func TestDuplicateToolPanics(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	reg := New(srv, Options{Mode: ModeOnly})
	reg.AddTool(&mcp.Tool{Name: "x"}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })
	defer func() {
		if recover() == nil {
			t.Error("no panic on duplicate")
		}
	}()
	reg.AddTool(&mcp.Tool{Name: "x"}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })
}
