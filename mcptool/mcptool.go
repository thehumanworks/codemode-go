// Package mcptool adds code mode to a server built on the official MCP Go
// SDK: one tool (default name "code") whose scripts call the server's other
// tools as async methods of a namespace object, discover them with search()
// and describe(), and return only what matters.
//
// Register tools through the Registry instead of directly on the
// *mcp.Server; the Registry forwards them to the server (in modes on and
// off) and records them for scripts (in modes on and only):
//
//	srv := mcp.NewServer(&mcp.Implementation{Name: "files"}, nil)
//	reg := mcptool.New(srv, mcptool.Options{Namespace: "files", Mode: mcptool.ModeOn})
//	mcptool.AddTool(reg, &mcp.Tool{Name: "read", Description: "Read a file"}, readHandler)
//	mcptool.AddTool(reg, &mcp.Tool{Name: "list", Description: "List a directory"}, listHandler)
//	reg.Register() // adds the "code" tool; call it after every AddTool
//
// A model then calls the code tool with:
//
//	const names = (await files.list({dir: "."})).entries;
//	const texts = await Promise.all(names.map(n => files.read({path: n})));
//	return texts.map(t => t.length);
package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	codemode "github.com/thehumanworks/codemode-go"
	"github.com/thehumanworks/codemode-go/catalog"
)

// Mode selects how the code tool and the other tools are exposed.
type Mode string

const (
	// ModeOn exposes the code tool beside the other tools.
	ModeOn Mode = "on"
	// ModeOnly exposes the code tool alone; scripts reach the other tools.
	ModeOnly Mode = "only"
	// ModeOff exposes the other tools and no code tool.
	ModeOff Mode = "off"
)

// ParseMode accepts "on", "only" and "off" (case-insensitive); "" means on.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeOn, nil
	case ModeOn, ModeOnly, ModeOff:
		return m, nil
	default:
		return "", fmt.Errorf("unknown code mode %q: use on, only, or off", s)
	}
}

// RunConfig is what Options.Prepare returns for one invocation of the code
// tool. The zero value changes nothing.
type RunConfig struct {
	// Context, when set, replaces the request context for the script and
	// every tool call it makes.
	Context context.Context
	// Entries are extra catalogue entries for this run, such as methods of
	// a remote API the host can forward to.
	Entries []catalog.Entry
	// Warning is attached to search and describe results when non-empty,
	// for example when Entries are incomplete.
	Warning string
	// Args rewrites a tool's arguments before the call, for example to
	// inject a default the script did not give. nil leaves them unchanged.
	Args func(tool string, args json.RawMessage) json.RawMessage
}

// Options configure a Registry. The zero value is usable.
type Options struct {
	// Name is the code tool's name. Default "code".
	Name string
	// Namespace is the JavaScript object holding the tools. Default "tools".
	Namespace string
	// StripPrefix is removed from tool names to form their member names:
	// with "rex_", the tool rex_capture is called as <ns>.capture. The full
	// name works too.
	StripPrefix string
	// Mode defaults to ModeOn.
	Mode Mode
	// Title and Description of the code tool. Description defaults to a
	// generated text that lists the members and explains discovery.
	Title       string
	Description string
	// Annotations for the code tool, if any.
	Annotations *mcp.ToolAnnotations
	// Params are extra properties on the code tool's input, beside code and
	// timeout. Their values reach Prepare.
	Params map[string]*jsonschema.Schema
	// Prepare runs before each script with the tool's arguments. It can
	// derive a context, add catalogue entries, and rewrite tool arguments.
	Prepare func(ctx context.Context, args map[string]any) (*RunConfig, error)
	// Catalog options; Namespace is overridden.
	Catalog catalog.Options
	// Sandbox options; Namespace and Hint are overridden, Timeout is the
	// default when the call gives none.
	Sandbox codemode.Options
	// MaxTimeout caps the timeout argument. Default 10 minutes.
	MaxTimeout time.Duration
	// MaxResultChars caps the result JSON shown to the model. Default 24 KiB.
	MaxResultChars int
	// TypesURI, when set, makes Register serve the TypeScript declarations
	// as a resource at this URI.
	TypesURI string
}

func (o Options) withDefaults() Options {
	if o.Name == "" {
		o.Name = "code"
	}
	if o.Namespace == "" {
		o.Namespace = "tools"
	}
	if o.Mode == "" {
		o.Mode = ModeOn
	}
	if o.Title == "" {
		o.Title = "Run a script"
	}
	if o.MaxTimeout <= 0 {
		o.MaxTimeout = 10 * time.Minute
	}
	if o.MaxResultChars <= 0 {
		o.MaxResultChars = 24 << 10
	}
	if o.Sandbox.Timeout <= 0 {
		o.Sandbox.Timeout = 60 * time.Second
	}
	return o
}

// entry is one recorded tool.
type entry struct {
	tool     *mcp.Tool
	member   string
	resolved *jsonschema.Resolved
	call     func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error)
}

// Registry records tools for scripts and forwards them to the server.
type Registry struct {
	server *mcp.Server
	opts   Options

	mu    sync.RWMutex
	tools []*entry
}

// New creates a Registry for server.
func New(server *mcp.Server, opts Options) *Registry {
	return &Registry{server: server, opts: opts.withDefaults()}
}

// Mode reports the effective mode.
func (r *Registry) Mode() Mode { return r.opts.Mode }

// AddTool records a typed tool and, unless the mode is only, registers it on
// the server exactly as mcp.AddTool would. When t.InputSchema is nil it is
// derived from In (and OutputSchema from Out when Out is not any), so the
// server and the catalogue share one schema. It panics on an invalid schema,
// like mcp.AddTool.
func AddTool[In, Out any](r *Registry, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if t.InputSchema == nil {
		if reflect.TypeFor[In]() == reflect.TypeFor[any]() {
			t.InputSchema = &jsonschema.Schema{Type: "object"}
		} else {
			t.InputSchema = mustSchema(t.Name, "input", reflect.TypeFor[In]())
		}
	}
	if t.OutputSchema == nil && reflect.TypeFor[Out]() != reflect.TypeFor[any]() {
		t.OutputSchema = mustSchema(t.Name, "output", reflect.TypeFor[Out]())
	}
	resolved := mustResolve(t.Name, t.InputSchema)
	isPointer := reflect.TypeFor[Out]().Kind() == reflect.Pointer
	call := func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		args, err := validateArgs(args, resolved)
		if err != nil {
			return nil, err
		}
		var in In
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		res, out, err := h(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: t.Name, Arguments: args}}, in)
		if err != nil {
			return nil, err
		}
		if res == nil {
			res = &mcp.CallToolResult{}
		}
		var outval any = out
		if isPointer && reflect.ValueOf(out).IsNil() {
			outval = nil
		}
		if outval != nil && res.StructuredContent == nil {
			b, err := json.Marshal(outval)
			if err != nil {
				return nil, fmt.Errorf("marshaling output: %w", err)
			}
			res.StructuredContent = json.RawMessage(b)
			if res.Content == nil {
				res.Content = []mcp.Content{&mcp.TextContent{Text: string(b)}}
			}
		}
		return res, nil
	}
	r.add(t, resolved, call)
	if r.opts.Mode != ModeOnly {
		mcp.AddTool(r.server, t, h)
	}
}

// AddTool records an untyped tool and, unless the mode is only, registers it
// on the server as (*mcp.Server).AddTool would. Arguments from scripts are
// validated when t.InputSchema is a *jsonschema.Schema.
func (r *Registry) AddTool(t *mcp.Tool, h mcp.ToolHandler) {
	var resolved *jsonschema.Resolved
	if sc, ok := t.InputSchema.(*jsonschema.Schema); ok {
		resolved = mustResolve(t.Name, sc)
	}
	call := func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		args, err := validateArgs(args, resolved)
		if err != nil {
			return nil, err
		}
		return h(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: t.Name, Arguments: args}})
	}
	r.add(t, resolved, call)
	if r.opts.Mode != ModeOnly {
		r.server.AddTool(t, h)
	}
}

func (r *Registry) add(t *mcp.Tool, resolved *jsonschema.Resolved, call func(context.Context, json.RawMessage) (*mcp.CallToolResult, error)) {
	member := t.Name
	if r.opts.StripPrefix != "" {
		member = strings.TrimPrefix(member, r.opts.StripPrefix)
	}
	member = catalog.Identifier(member)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.tools {
		if e.tool.Name == t.Name {
			panic(fmt.Sprintf("mcptool: tool %q added twice", t.Name))
		}
	}
	r.tools = append(r.tools, &entry{tool: t, member: member, resolved: resolved, call: call})
}

func mustSchema(tool, which string, rt reflect.Type) *jsonschema.Schema {
	if rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	s, err := jsonschema.ForType(rt, &jsonschema.ForOptions{})
	if err != nil {
		panic(fmt.Sprintf("mcptool: tool %q: %s schema: %v", tool, which, err))
	}
	return s
}

func mustResolve(tool string, schema any) *jsonschema.Resolved {
	var sc *jsonschema.Schema
	switch v := schema.(type) {
	case *jsonschema.Schema:
		sc = v
	default:
		b, err := json.Marshal(v)
		if err == nil {
			err = json.Unmarshal(b, &sc)
		}
		if err != nil {
			panic(fmt.Sprintf("mcptool: tool %q: input schema: %v", tool, err))
		}
	}
	resolved, err := sc.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		panic(fmt.Sprintf("mcptool: tool %q: input schema: %v", tool, err))
	}
	return resolved
}

// validateArgs checks a script's arguments against the schema and applies
// defaults, as the SDK does for calls over the wire.
func validateArgs(args json.RawMessage, resolved *jsonschema.Resolved) (json.RawMessage, error) {
	s := strings.TrimSpace(string(args))
	if s == "" || s == "null" || s == "undefined" {
		args = json.RawMessage("{}")
	}
	if resolved == nil {
		return args, nil
	}
	var inst any = map[string]any{}
	if err := json.Unmarshal(args, &inst); err != nil {
		return nil, fmt.Errorf("invalid arguments: must be a JSON object: %w", err)
	}
	if _, ok := inst.(map[string]any); !ok {
		return nil, errors.New("invalid arguments: must be an object with the tool's properties")
	}
	if err := resolved.ApplyDefaults(&inst); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if err := resolved.Validate(&inst); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	b, err := json.Marshal(inst)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Tools returns the recorded tools in registration order.
func (r *Registry) Tools() []*mcp.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*mcp.Tool, 0, len(r.tools))
	for _, e := range r.tools {
		out = append(out, e.tool)
	}
	return out
}

func (r *Registry) find(name string) *entry {
	name = strings.TrimPrefix(name, r.opts.Namespace+".")
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.tools {
		if e.tool.Name == name || e.member == name {
			return e
		}
	}
	return nil
}

func (r *Registry) findByName(name string) *entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.tools {
		if e.tool.Name == name {
			return e
		}
	}
	return nil
}

// Call invokes a recorded tool the way a script does: by full name, member
// name, or <namespace>.<member>; arguments validated; result unwrapped.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	e := r.find(name)
	if e == nil {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return UnwrapResult(e.call(ctx, args))
}

// Catalog builds the discovery catalogue: one entry per recorded tool at
// <namespace>.<member>, plus extra.
func (r *Registry) Catalog(extra ...catalog.Entry) *catalog.Catalog {
	return r.catalogWith("", extra)
}

func (r *Registry) catalogWith(warning string, extra []catalog.Entry) *catalog.Catalog {
	r.mu.RLock()
	entries := make([]catalog.Entry, 0, len(r.tools)+len(extra))
	for _, e := range r.tools {
		entries = append(entries, catalog.Entry{
			Path:        r.opts.Namespace + "." + e.member,
			Kind:        "tool",
			Description: describeTool(e.tool),
			Input:       e.tool.InputSchema,
			Output:      e.tool.OutputSchema,
			Keywords:    e.tool.Name,
		})
	}
	r.mu.RUnlock()
	entries = append(entries, extra...)
	opts := r.opts.Catalog
	opts.Namespace = r.opts.Namespace
	if warning != "" {
		opts.Warning = warning
	}
	return catalog.New(entries, opts)
}

func describeTool(t *mcp.Tool) string {
	title := strings.TrimSpace(t.Title)
	desc := strings.TrimSpace(t.Description)
	switch {
	case title == "":
		return desc
	case desc == "":
		return title
	default:
		return strings.TrimSuffix(title, ".") + ". " + desc
	}
}

// Declarations renders the TypeScript the model can rely on: the search and
// describe globals and the namespace with every recorded tool.
func (r *Registry) Declarations() string {
	return "// Globals available to scripts.\n" + catalog.GlobalsDeclaration() + "\n\n" + r.Catalog().Declaration()
}

// Guidance returns a paragraph for the server's instructions that tells the
// model when and how to use the code tool. Empty in ModeOff.
func (r *Registry) Guidance() string {
	ns, name := r.opts.Namespace, r.opts.Name
	first := "<tool>"
	if tools := r.Tools(); len(tools) > 0 {
		first = r.find(tools[0].Name).member
	}
	switch r.opts.Mode {
	case ModeOn:
		return fmt.Sprintf("`%s` runs JavaScript that calls these same tools as `%s.<name>(args)` (for example `%s.%s`) with `await`, and discovers more with `await search(\"...\")` and `await describe(\"%s.%s\")`. Prefer it when a task needs several calls: inspect then act, loop over items, wait and verify. Single actions can use the tools directly.", name, ns, ns, first, ns, first)
	case ModeOnly:
		return fmt.Sprintf("Everything happens through `%s`: it runs JavaScript in which the tools are async methods on `%s` (`await %s.%s({...})`). Start by calling `await describe(\"%s\")` to see the tool signatures, or `await search(\"what you need\")` to find a tool, then write the script that does the work and returns a summary.", name, ns, ns, first, ns)
	default:
		return ""
	}
}

func (r *Registry) description() string {
	if r.opts.Description != "" {
		return r.opts.Description
	}
	ns := r.opts.Namespace
	r.mu.RLock()
	names := make([]string, 0, len(r.tools))
	for _, e := range r.tools {
		names = append(names, e.member)
	}
	r.mu.RUnlock()
	first := "<tool>"
	if len(names) > 0 {
		first = names[0]
	}
	return fmt.Sprintf("Run JavaScript that calls the tools of this server. The code is the body of an async function in a sandbox (no network, filesystem, timers, or imports); only its return value and console output come back, so filter and summarise data in code instead of returning it raw.\n\n"+
		"Tools are async methods on the global `%s`, each taking one object argument: %s. Example: `const r = await %s.%s({...}); return r;`. Independent calls run in parallel with Promise.all. A failed call throws an Error with the tool's message; catch it when a fallback exists.\n\n"+
		"Discover from inside the script, not from memory: `await search(\"what you need\")` returns matching tools and methods with their paths; `await describe(path)` returns TypeScript types and how to call it (describe(\"%s\") lists every tool). The result of describe is data to read; print it with console.log or return it, then write the next script.\n\n"+
		"Use this for anything that takes more than one step: inspect-then-act, loops, waiting on something, or building a report. Verify outcomes in the same script.", ns, strings.Join(names, ", "), ns, first, ns)
}

func (r *Registry) inputSchema() *jsonschema.Schema {
	props := map[string]*jsonschema.Schema{
		"code":    {Type: "string", Description: "JavaScript source, run as the body of an async function. Use await on every tool call; return the value to report; console.log for notes."},
		"timeout": {Type: "string", Description: fmt.Sprintf("Wall-clock limit for the whole script, e.g. 2m. Default %s, max %s. Raise it when the script waits on long operations.", r.opts.Sandbox.Timeout, r.opts.MaxTimeout)},
	}
	for k, v := range r.opts.Params {
		if k != "code" && k != "timeout" {
			props[k] = v
		}
	}
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"code"}}
}

// Register adds the code tool, and the types resource when TypesURI is set.
// It does nothing in ModeOff. Call it after every AddTool, since the
// generated description lists the members.
func (r *Registry) Register() {
	if r.opts.Mode == ModeOff {
		return
	}
	r.server.AddTool(&mcp.Tool{
		Name:        r.opts.Name,
		Title:       r.opts.Title,
		Description: r.description(),
		InputSchema: r.inputSchema(),
		Annotations: r.opts.Annotations,
	}, r.handle)
	if r.opts.TypesURI != "" {
		r.server.AddResource(&mcp.Resource{
			URI:         r.opts.TypesURI,
			Name:        "code-types",
			Title:       "TypeScript declarations for " + r.opts.Name,
			Description: fmt.Sprintf("The %s namespace that scripts passed to %s can call, as TypeScript declarations.", r.opts.Namespace, r.opts.Name),
			MIMEType:    "text/x.typescript",
		}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/x.typescript", Text: r.Declarations()}}}, nil
		})
	}
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}
}

// handle is the code tool's handler.
func (r *Registry) handle(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult("arguments must be a JSON object"), nil
		}
	}
	code, _ := args["code"].(string)
	if strings.TrimSpace(code) == "" {
		return errResult("code is required: the body of an async function, for example `return await " + r.opts.Namespace + "." + "<tool>({...})`"), nil
	}
	wait, err := parseTimeout(args["timeout"], r.opts.Sandbox.Timeout)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if wait > r.opts.MaxTimeout {
		wait = r.opts.MaxTimeout
	}
	cfg := &RunConfig{}
	if r.opts.Prepare != nil {
		c, err := r.opts.Prepare(ctx, args)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if c != nil {
			cfg = c
		}
	}
	runCtx := ctx
	if cfg.Context != nil {
		runCtx = cfg.Context
	}
	cat := r.catalogWith(cfg.Warning, cfg.Entries)
	sopts := r.opts.Sandbox
	sopts.Namespace = r.opts.Namespace
	sopts.Hint = cat.Hint()
	sopts.Timeout = wait
	out, err := codemode.Run(runCtx, code, r.sandboxTools(cfg.Args), r.globals(cat), sopts)
	return r.render(out, err, wait), nil
}

// globals returns the catalogue's search and describe, with describe also
// accepting a tool's full name (rex_capture) for its member path.
func (r *Registry) globals(cat *catalog.Catalog) map[string]codemode.HostFunc {
	g := cat.Globals()
	describe := g["describe"]
	g["describe"] = func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var pos []json.RawMessage
		if json.Unmarshal(args, &pos) == nil && len(pos) > 0 {
			var target string
			if json.Unmarshal(pos[0], &target) == nil {
				// Only full names are mapped here; member names and
				// paths are the catalogue's business, so a member that
				// shares its name with a group (api vs api.*) still lists
				// the group.
				if e := r.findByName(strings.TrimSpace(target)); e != nil && e.tool.Name != e.member {
					path, _ := json.Marshal(r.opts.Namespace + "." + e.member)
					pos[0] = path
					args, _ = json.Marshal(pos)
				}
			}
		}
		return describe(ctx, args)
	}
	return g
}

func (r *Registry) sandboxTools(rewrite func(string, json.RawMessage) json.RawMessage) []codemode.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make([]codemode.Tool, 0, len(r.tools))
	for _, e := range r.tools {
		e := e
		tools = append(tools, codemode.Tool{Name: e.tool.Name, JSName: e.member, Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			if rewrite != nil {
				args = rewrite(e.tool.Name, args)
			}
			return UnwrapResult(e.call(ctx, args))
		}})
	}
	return tools
}

// parseTimeout accepts "30s", "2m", a number of seconds, or nothing.
func parseTimeout(v any, def time.Duration) (time.Duration, error) {
	switch t := v.(type) {
	case nil:
		return def, nil
	case float64:
		if t <= 0 {
			return def, nil
		}
		return time.Duration(t * float64(time.Second)), nil
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return def, nil
		}
		if n, err := strconv.ParseFloat(t, 64); err == nil {
			if n <= 0 {
				return def, nil
			}
			return time.Duration(n * float64(time.Second)), nil
		}
		d, err := time.ParseDuration(t)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("invalid timeout %q: use forms like 30s, 2m, or a number of seconds", t)
		}
		return d, nil
	default:
		return 0, fmt.Errorf("invalid timeout %v: use forms like 30s, 2m, or a number of seconds", v)
	}
}

// render turns a run into the tool result: a text report for the model and
// structured content for clients that read it.
func (r *Registry) render(out codemode.Output, err error, wait time.Duration) *mcp.CallToolResult {
	var b strings.Builder
	structured := map[string]any{}
	var logs []string
	var scriptErr *codemode.ScriptError
	var timeoutErr *codemode.TimeoutError
	switch {
	case err == nil:
		text, truncated := codemode.Clip(string(out.Result), r.opts.MaxResultChars)
		fmt.Fprintf(&b, "Script completed in %s with %d tool call(s).\n", out.Elapsed.Round(time.Millisecond), out.Calls)
		if string(out.Result) != "null" {
			b.WriteString("<result>\n" + text + "\n</result>\n")
		} else {
			b.WriteString("The script returned nothing. Return a value to report results.\n")
		}
		structured["ok"] = true
		structured["result"] = out.Result
		structured["calls"] = out.Calls
		structured["elapsed_ms"] = out.Elapsed.Milliseconds()
		structured["truncated"] = truncated
		logs = out.Logs
	case errors.As(err, &scriptErr):
		fmt.Fprintf(&b, "Script failed after %s with %d tool call(s):\n%s\n", scriptErr.Elapsed.Round(time.Millisecond), scriptErr.Calls, scriptErr.Message)
		structured["ok"] = false
		structured["error"] = scriptErr.Message
		structured["calls"] = scriptErr.Calls
		structured["elapsed_ms"] = scriptErr.Elapsed.Milliseconds()
		logs = scriptErr.Logs
	case errors.As(err, &timeoutErr):
		fmt.Fprintf(&b, "Script timed out after %s with %d tool call(s) started. Pass a longer timeout (max %s), or split the work across scripts.\n", wait, timeoutErr.Calls, r.opts.MaxTimeout)
		structured["ok"] = false
		structured["error"] = "timeout"
		structured["calls"] = timeoutErr.Calls
		structured["elapsed_ms"] = timeoutErr.Elapsed.Milliseconds()
		logs = timeoutErr.Logs
	default:
		fmt.Fprintf(&b, "Sandbox error: %v\n", err)
		structured["ok"] = false
		structured["error"] = err.Error()
	}
	if len(logs) > 0 {
		b.WriteString("<console_output>\n" + strings.Join(logs, "\n") + "\n</console_output>\n")
		structured["logs"] = logs
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: strings.TrimRight(b.String(), "\n")}},
		StructuredContent: structured,
		IsError:           err != nil,
	}
}

// UnwrapResult turns a tool result into the value a script receives, the
// way Cloudflare's codemode does: an error or IsError rejects with the
// message, structured content wins, a single text that parses as a JSON
// object or array becomes that value, a single other text becomes a string,
// and several texts become a string array.
func UnwrapResult(res *mcp.CallToolResult, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	if res == nil {
		return json.RawMessage("null"), nil
	}
	texts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	if res.IsError {
		msg := strings.Join(texts, "\n")
		if msg == "" {
			msg = "tool failed"
		}
		return nil, errors.New(msg)
	}
	if res.StructuredContent != nil {
		return json.Marshal(res.StructuredContent)
	}
	switch len(texts) {
	case 0:
		return json.RawMessage("null"), nil
	case 1:
		t := strings.TrimSpace(texts[0])
		if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
			return json.RawMessage(t), nil
		}
		return json.Marshal(texts[0])
	default:
		return json.Marshal(texts)
	}
}
