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
//	reg.Register() // adds the "code" tool; tools added later refresh it
//
// A model then calls the code tool with:
//
//	const names = (await files.list({dir: "."})).entries;
//	const texts = await Promise.all(names.map(n => files.read({path: n})));
//	return texts.map(t => t.length);
//
// A tool called from a script behaves as the same tool called by a client:
// typed tools (AddTool) get the SDK's input validation and defaults, its
// case-sensitive decoding of In, and its output validation and result
// shaping; untyped tools ((*Registry).AddTool) get the arguments as sent,
// with no validation. The handler's request carries the Session and Extra
// of the code tool's request. The differences: argument errors read
// "invalid arguments: ..." rather than the SDK's "validating \"arguments\"",
// and a handler that panics rejects the script's call with an error instead
// of crashing the process.
package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	// Cleanup, when set, is called once the script run has finished,
	// whatever its outcome, for example to cancel a Context derived with
	// context.WithTimeout. It is not called when Prepare returns an error.
	Cleanup func()
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
	// timeout. A value given for one is validated against its schema
	// ("invalid arguments: ..." otherwise) and then reaches Prepare. New
	// panics when one of the schemas cannot be resolved.
	Params map[string]*jsonschema.Schema
	// Prepare runs before each script with the tool's arguments. It can
	// derive a context, add catalogue entries, and rewrite tool arguments.
	Prepare func(ctx context.Context, args map[string]any) (*RunConfig, error)
	// Catalog options; Namespace is overridden.
	Catalog catalog.Options
	// Sandbox options; Namespace and Hint are overridden, Timeout is the
	// default when the call gives none (at most MaxTimeout).
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
	if o.Sandbox.Timeout > o.MaxTimeout {
		o.Sandbox.Timeout = o.MaxTimeout
	}
	return o
}

// entry is one recorded tool.
type entry struct {
	tool   *mcp.Tool // the registry's copy
	member string
	call   func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error)
}

// Registry records tools for scripts and forwards them to the server.
type Registry struct {
	server *mcp.Server
	opts   Options
	params map[string]*jsonschema.Resolved

	mu         sync.RWMutex
	tools      []*entry
	registered bool // Register has run; later tools refresh the code tool

	codeMu sync.Mutex // serialises registrations of the code tool
}

// New creates a Registry for server. It panics when a schema in
// opts.Params cannot be resolved.
func New(server *mcp.Server, opts Options) *Registry {
	r := &Registry{server: server, opts: opts.withDefaults()}
	for name, s := range r.opts.Params {
		if s == nil || name == "code" || name == "timeout" {
			continue
		}
		resolved, err := s.Resolve(nil)
		if err != nil {
			panic(fmt.Sprintf("mcptool: param %q: %v", name, err))
		}
		if r.params == nil {
			r.params = map[string]*jsonschema.Resolved{}
		}
		r.params[name] = resolved
	}
	return r
}

// Mode reports the effective mode.
func (r *Registry) Mode() Mode { return r.opts.Mode }

// AddTool records a typed tool and, unless the mode is only, registers it on
// the server exactly as mcp.AddTool would. Like mcp.AddTool it works on a
// copy of t: when the copy's InputSchema is nil it is derived from In (and
// OutputSchema from Out when Out is not any), so the server and the
// catalogue share one schema; t itself is not modified. Scripts get what a
// client gets: input validated with defaults applied, In decoded with
// case-sensitive field names, output validated against the output schema
// with defaults applied, and the result shaped the same way.
//
// It panics, in every mode and before recording anything, where mcp.AddTool
// would (for example when the input schema is not an object schema), and
// when the tool's name or member name collides with another tool's or with
// the code tool's name.
func AddTool[In, Out any](r *Registry, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	tt := *t
	if reflect.TypeFor[In]() == reflect.TypeFor[any]() && tt.InputSchema == nil {
		tt.InputSchema = &jsonschema.Schema{Type: "object"}
	}
	inResolved, _, err := setSchema[In](&tt.InputSchema)
	if err != nil {
		panic(fmt.Sprintf("AddTool: tool %q: input schema: %v", t.Name, err))
	}
	var (
		outResolved *jsonschema.Resolved
		elemZero    any
	)
	if tt.OutputSchema != nil || reflect.TypeFor[Out]() != reflect.TypeFor[any]() {
		if outResolved, elemZero, err = setSchema[Out](&tt.OutputSchema); err != nil {
			panic(fmt.Sprintf("AddTool: tool %q: output schema: %v", t.Name, err))
		}
	}
	sdkCheck(func(s *mcp.Server) { mcp.AddTool(s, &tt, h) })
	call := func(ctx context.Context, args json.RawMessage) (res *mcp.CallToolResult, err error) {
		defer recoverCall(tt.Name, &res, &err)
		args = objectArgs(args)
		input, err := applySchema(args, inResolved, false)
		if err != nil {
			return nil, fmt.Errorf("invalid arguments: %v", err)
		}
		var in In
		if err := decodeInput(input, &in); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		res, out, err := h(ctx, toolRequest(ctx, tt.Name, args), in)
		if err != nil {
			return nil, err
		}
		return shapeResult(res, out, elemZero, outResolved)
	}
	r.add(&tt, call, func() { mcp.AddTool(r.server, &tt, h) })
}

// AddTool records an untyped tool and, unless the mode is only, registers it
// on the server as (*mcp.Server).AddTool would, using a copy of t. As over
// the wire, the handler gets the arguments as the script sent them (an
// empty object when none) with no validation and no defaults, and its
// result is used as is. It panics where (*mcp.Server).AddTool would, in
// every mode, and on name collisions like AddTool.
func (r *Registry) AddTool(t *mcp.Tool, h mcp.ToolHandler) {
	tt := *t
	sdkCheck(func(s *mcp.Server) { s.AddTool(&tt, h) })
	call := func(ctx context.Context, args json.RawMessage) (res *mcp.CallToolResult, err error) {
		defer recoverCall(tt.Name, &res, &err)
		return h(ctx, toolRequest(ctx, tt.Name, objectArgs(args)))
	}
	r.add(&tt, call, func() { r.server.AddTool(&tt, h) })
}

// add records a checked tool and, unless the mode is only, registers it on
// the server. When Register has already run, the code tool is registered
// again so its description lists the new member.
func (r *Registry) add(t *mcp.Tool, call func(context.Context, json.RawMessage) (*mcp.CallToolResult, error), register func()) {
	member := t.Name
	if r.opts.StripPrefix != "" {
		member = strings.TrimPrefix(member, r.opts.StripPrefix)
	}
	member = catalog.Identifier(member)
	refresh := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := r.collision(t.Name, member); err != "" {
			panic("mcptool: " + err)
		}
		if r.opts.Mode != ModeOnly {
			register()
		}
		r.tools = append(r.tools, &entry{tool: t, member: member, call: call})
		return r.registered
	}()
	if refresh {
		r.registerCode()
	}
}

// collision describes why a tool cannot be added, or returns "". Scripts
// reach a tool by its name and by its member name, so neither may equal
// another tool's name or member, nor the code tool's name.
func (r *Registry) collision(name, member string) string {
	if name == r.opts.Name || member == r.opts.Name {
		return fmt.Sprintf("tool %q (member %q) collides with the code tool %q", name, member, r.opts.Name)
	}
	for _, e := range r.tools {
		if e.tool.Name == name {
			return fmt.Sprintf("tool %q added twice", name)
		}
		if e.member == member || e.member == name || e.tool.Name == member {
			return fmt.Sprintf("tool %q (member %q) collides with tool %q (member %q)", name, member, e.tool.Name, e.member)
		}
	}
	return ""
}

// validateParams checks the values given for Options.Params.
func (r *Registry) validateParams(args map[string]any) error {
	for name, resolved := range r.params {
		v, ok := args[name]
		if !ok {
			continue
		}
		if err := resolved.Validate(&v); err != nil {
			return fmt.Errorf("invalid arguments: %s: %v", name, err)
		}
	}
	return nil
}

// Tools returns the recorded tools in registration order. They are the
// registry's copies and must not be modified.
func (r *Registry) Tools() []*mcp.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*mcp.Tool, 0, len(r.tools))
	for _, e := range r.tools {
		out = append(out, e.tool)
	}
	return out
}

// find resolves what a caller may name a tool: its full name, its member
// name, or <namespace>.<member>. The name as given is tried first, so a
// full name that contains a dot ("gh.search") is found.
func (r *Registry) find(name string) *entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	match := func(name string) *entry {
		for _, e := range r.tools {
			if e.tool.Name == name || e.member == name {
				return e
			}
		}
		return nil
	}
	if e := match(name); e != nil {
		return e
	}
	if rest, ok := strings.CutPrefix(name, r.opts.Namespace+"."); ok {
		return match(rest)
	}
	return nil
}

// findByName finds a tool by its full name, also written as
// <namespace>.<full name>.
func (r *Registry) findByName(name string) *entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	match := func(name string) *entry {
		for _, e := range r.tools {
			if e.tool.Name == name {
				return e
			}
		}
		return nil
	}
	if e := match(name); e != nil {
		return e
	}
	if rest, ok := strings.CutPrefix(name, r.opts.Namespace+"."); ok {
		return match(rest)
	}
	return nil
}

// Call invokes a recorded tool the way a script does: by full name, member
// name, or <namespace>.<member>; the result unwrapped as a script receives
// it. The request passed to the handler carries no Session or Extra unless
// ctx comes from a run of the code tool, so a handler called through Call
// must cope with a nil req.Session. A handler that panics makes Call return
// an error.
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
	r.mu.RLock()
	if len(r.tools) > 0 {
		first = r.tools[0].member
	}
	r.mu.RUnlock()
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
// It does nothing in ModeOff. A tool added after Register registers the
// code tool again (the server replaces it and notifies clients), so the
// generated description always lists every member.
func (r *Registry) Register() {
	if r.opts.Mode == ModeOff {
		return
	}
	r.mu.Lock()
	r.registered = true
	r.mu.Unlock()
	r.registerCode()
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

// registerCode adds or replaces the code tool on the server.
func (r *Registry) registerCode() {
	r.codeMu.Lock()
	defer r.codeMu.Unlock()
	r.server.AddTool(&mcp.Tool{
		Name:        r.opts.Name,
		Title:       r.opts.Title,
		Description: r.description(),
		InputSchema: r.inputSchema(),
		Annotations: r.opts.Annotations,
	}, r.handle)
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
	if args == nil { // "null"
		args = map[string]any{}
	}
	code, _ := args["code"].(string)
	if strings.TrimSpace(code) == "" {
		return errResult("code is required: the body of an async function, for example `return await " + r.opts.Namespace + "." + "<tool>({...})`"), nil
	}
	wait, err := parseTimeout(args["timeout"], r.opts.Sandbox.Timeout, r.opts.MaxTimeout)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := r.validateParams(args); err != nil {
		return errResult(err.Error()), nil
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
	if cfg.Cleanup != nil {
		defer cfg.Cleanup()
	}
	runCtx := ctx
	if cfg.Context != nil {
		runCtx = cfg.Context
	}
	// Tool calls made by the script carry this request's Session and Extra.
	runCtx = context.WithValue(runCtx, requestKey{}, req)
	cat := r.catalogWith(cfg.Warning, cfg.Entries)
	sopts := r.opts.Sandbox
	sopts.Namespace = r.opts.Namespace
	sopts.Hint = cat.Hint()
	sopts.Timeout = wait
	out, err := codemode.Run(runCtx, code, r.sandboxTools(cfg.Args), r.globals(cat), sopts)
	return r.render(out, err, wait), nil
}

// globals returns the catalogue's search and describe, with describe also
// accepting a tool's full name (rex_capture, or tools.rex_capture) for its
// member path.
func (r *Registry) globals(cat *catalog.Catalog) map[string]codemode.HostFunc {
	g := cat.Globals()
	describe := g["describe"]
	g["describe"] = func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var pos []json.RawMessage
		if json.Unmarshal(args, &pos) == nil && len(pos) > 0 {
			var target string
			if json.Unmarshal(pos[0], &target) == nil {
				// Only full names (alone or after the namespace) are
				// mapped here; member names and paths are the
				// catalogue's business, so a member that shares its name
				// with a group (api vs api.*) still lists the group.
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

// parseTimeout accepts "30s", "2m", a number of seconds, or nothing, and
// returns a duration in [1ns, limit]. Nothing, zero, and negative numbers
// mean def; NaN, infinities, negative durations and other values are errors.
func parseTimeout(v any, def, limit time.Duration) (time.Duration, error) {
	invalid := func() error {
		return fmt.Errorf("invalid timeout %v: use forms like 30s, 2m, or a number of seconds", v)
	}
	var secs float64
	switch t := v.(type) {
	case nil:
		return def, nil
	case float64:
		secs = t
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return def, nil
		}
		n, err := strconv.ParseFloat(t, 64)
		if err != nil {
			d, err := time.ParseDuration(t)
			if err != nil || d < 0 {
				return 0, fmt.Errorf("invalid timeout %q: use forms like 30s, 2m, or a number of seconds", t)
			}
			if d == 0 {
				return def, nil
			}
			return min(d, limit), nil
		}
		secs = n
	default:
		return 0, invalid()
	}
	if math.IsNaN(secs) || math.IsInf(secs, 0) {
		return 0, invalid()
	}
	if secs <= 0 {
		return def, nil
	}
	ns := secs * float64(time.Second)
	if ns >= float64(limit) {
		return limit, nil
	}
	return max(time.Duration(ns), time.Nanosecond), nil
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
		if truncated {
			structured["result"] = text // the clipped JSON, as a string
		} else {
			structured["result"] = out.Result
		}
		structured["calls"] = out.Calls
		structured["elapsed_ms"] = out.Elapsed.Milliseconds()
		structured["truncated"] = truncated
		logs = out.Logs
	case errors.As(err, &scriptErr):
		msg, _ := codemode.Clip(scriptErr.Message, r.opts.MaxResultChars)
		fmt.Fprintf(&b, "Script failed after %s with %d tool call(s):\n%s\n", scriptErr.Elapsed.Round(time.Millisecond), scriptErr.Calls, msg)
		structured["ok"] = false
		structured["error"] = msg
		structured["calls"] = scriptErr.Calls
		structured["elapsed_ms"] = scriptErr.Elapsed.Milliseconds()
		logs = scriptErr.Logs
	case errors.As(err, &timeoutErr):
		took := timeoutErr.Elapsed.Round(time.Millisecond)
		if timeoutErr.Timeout >= wait {
			// The script's own timeout fired.
			fmt.Fprintf(&b, "Script timed out after %s (limit %s) with %d tool call(s) started. Pass a longer timeout (max %s), or split the work across scripts.\n", took, wait, timeoutErr.Calls, r.opts.MaxTimeout)
		} else {
			// The request's context had an earlier deadline.
			fmt.Fprintf(&b, "Script timed out after %s with %d tool call(s) started: the request's deadline passed before the script's %s timeout. A longer timeout will not help; split the work across scripts.\n", took, timeoutErr.Calls, wait)
		}
		structured["ok"] = false
		structured["error"] = "timeout"
		structured["calls"] = timeoutErr.Calls
		structured["elapsed_ms"] = timeoutErr.Elapsed.Milliseconds()
		logs = timeoutErr.Logs
	default:
		msg, _ := codemode.Clip(err.Error(), r.opts.MaxResultChars)
		fmt.Fprintf(&b, "Sandbox error: %s\n", msg)
		structured["ok"] = false
		structured["error"] = msg
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
// message, structured content wins (a JSON null counts as none, as it does
// once sent over the wire), a single text that parses as a JSON object or
// array becomes that value, a single other text becomes a string, several
// texts become a string array, and content without text (images, audio,
// resources) becomes the JSON of the content array.
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
		b, err := json.Marshal(res.StructuredContent)
		if err != nil || string(b) != "null" {
			return b, err
		}
	}
	switch len(texts) {
	case 0:
		if len(res.Content) > 0 {
			return json.Marshal(res.Content)
		}
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
