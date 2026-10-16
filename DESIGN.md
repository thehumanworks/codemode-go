# codemode-go design contract

Module `github.com/thehumanworks/codemode-go`. Three packages, each usable on
its own, each depending only on the one below it:

| package   | import path                                  | depends on            | purpose |
|-----------|----------------------------------------------|-----------------------|---------|
| `codemode`| `github.com/thehumanworks/codemode-go`        | fastschema/qjs        | run agent-written JavaScript in a QuickJS sandbox; scripts call Go functions as async methods of a namespace object |
| `catalog` | `github.com/thehumanworks/codemode-go/catalog`| `codemode`            | incremental discovery: BM25 `search(query)` and `describe(path)` host globals, JSON Schema to TypeScript declarations |
| `mcptool` | `github.com/thehumanworks/codemode-go/mcptool`| `catalog`, MCP Go SDK | register one `code` tool on a `*mcp.Server` whose scripts call the server's other tools, in modes on / only / off |

The standard being followed is Cloudflare's `@cloudflare/codemode` and pi's
`@earendil-works/pi-codemode`: one code tool, tools as async methods on a
namespace object, `search` and `describe` callable from inside the script,
only the return value and console output reach the model, results clipped.

No package may import anything from a host application. `catalog` must not
import the MCP SDK or jsonschema-go (schemas arrive as `any` and are
normalised through a JSON round trip). `mcptool` is the only package that
knows about MCP.

## package codemode (module root)

```go
// HostFunc handles one call from a script. args is the JSON the script passed:
// an object for tools, an array of positional arguments for globals. The
// returned JSON becomes the resolved value ("null" when empty); an error rejects.
type HostFunc func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

// Tool is one method of the namespace object. Scripts call both ns.JSName and ns.Name.
type Tool struct { Name, JSName string; Call HostFunc }

type Options struct {
    Namespace      string        // global object holding the tools; default "tools"
    Timeout        time.Duration // wall-clock limit; default 60s; an earlier ctx deadline wins
    MemoryLimit    int           // VM heap bytes; default 64 MiB
    MaxCalls       int           // host calls per script; default 200
    MaxConcurrency int           // host calls in flight; default 8
    MaxLogChars    int           // captured console chars; default 64 KiB; once reached, later lines are dropped
    Hint           string        // appended to "ns.x does not exist" errors (catalog.Hint supplies one)
    OnLog          func(line string) // receives each kept console line as it is printed; optional
}

type Output struct { Result json.RawMessage; Logs []string; Calls int; Elapsed time.Duration }

// ScriptError: thrown value, syntax error, stall, out of memory, call limit, VM stopped (exit).
type ScriptError struct { Message string; Logs []string; Calls int; Elapsed time.Duration }
var ErrTimeout = errors.New("script timed out")
// TimeoutError unwraps to ErrTimeout. Logs printed before the deadline are kept.
// Timeout is the limit that applied: Options.Timeout or the shorter ctx deadline.
type TimeoutError struct { Timeout time.Duration; Logs []string; Calls int; Elapsed time.Duration }

func Warm()                                   // compile the wasm module, create the empty root, self-test the deadline; Run calls it
func Run(ctx context.Context, code string, tools []Tool, globals map[string]HostFunc, opts Options) (Output, error)
type Runner struct{ /* static tools, globals, options */ }
func NewRunner(tools []Tool, globals map[string]HostFunc, opts Options) *Runner
func (r *Runner) Run(ctx context.Context, code string) (Output, error)
func Clip(s string, max int) (string, bool)   // head+tail with a TRUNCATED marker
```

Semantics:

- `code` is the body of an async function. `return` ends it; the return value
  is serialised with `JSON.stringify` (`undefined` becomes `null`); a value
  that cannot be serialised is a `ScriptError` mentioning "serialised".
- Globals are top-level async functions taking positional arguments, passed
  to the `HostFunc` as a JSON array.
- Unknown namespace members throw `TypeError: ns.x does not exist. Did you
  mean ns.y? <Hint>`.
- A script that awaits a promise nothing can settle fails with a
  `ScriptError` containing "stalled".
- If ctx is cancelled (not a deadline) the error wraps `context.Canceled`.
  A deadline, from ctx or from Timeout, gives `*TimeoutError`.
- In-flight host calls are cancelled through their ctx when the script ends.
  Run waits one grace period (1s) for them; a HostFunc that ignores its ctx
  leaks. A HostFunc that panics rejects the call with "tool panicked: ...".
- Calls over `MaxCalls`, unknown names and non-JSON arguments are rejected
  synchronously by the host (the promise rejects); they never occupy the
  result channel, so a fire-and-forget burst cannot wedge a run. `Calls`
  counts every attempt.
- Console lines are captured on the host side, so they survive timeouts and
  out-of-memory; `OnLog` sees the kept lines live. When a line would exceed
  the cap it is cut (or dropped when little room is left), one marker line
  `--- further console output dropped (limit N chars) ---` is added, and
  every later line is dropped.
- The return value reaches the host through a closure-held host function the
  driver calls once (first report wins) and is checked with `json.Valid`;
  a script cannot forge it through a global.
- The prelude removes `__host_*`, the quickjs-libc globals (`std`, `os`,
  `bjson`, `print`, `setTimeout`, `setInterval`, `clearTimeout`,
  `clearInterval`, `scriptArgs`, `gc`) and qjs's `QJS_PROXY_VALUE` from
  `globalThis`, pins `Function.prototype.call`/`apply`, and `__drive` removes
  itself on first use. The VM's filesystem root is an empty read-only
  directory, its stdout/stderr go to `io.Discard`, it has no environment and
  no network. A dynamic `import("qjs:os")`/`import("qjs:std")` still works
  (the module loader is C-level): it can list the empty root, fail to write,
  sleep, set libc timers, or call `std.exit`. Exit becomes a `ScriptError`
  mentioning `exit(N)`; a sleep or timer cannot be interrupted, so Run
  returns a `TimeoutError` after the deadline plus a grace period (1s) and
  the VM goroutine ends when the sleep does.
- The script starts on line 1 of `script.js`, so stack traces match the
  model's code; prelude frames are stripped.

## package catalog

```go
// Entry is one discoverable thing: a namespace member, an API method, anything.
type Entry struct {
    Path        string         // "rex.capture", "api.session.view"; dots separate groups
    Kind        string         // free-form: "tool", "api_method", ...
    Description string
    Input       any            // JSON Schema of the single argument (map, *jsonschema.Schema, raw JSON as json.RawMessage/[]byte/string); nil = none
    Output      any            // JSON Schema of the result; nil = unknown
    Extra       map[string]any // merged into describe() output for this entry, e.g. "call", examples
    Keywords    string         // extra searchable text
}

type Options struct {
    Namespace            string // the sandbox namespace; entries at "<ns>.<name>" are its members
    NamespaceDescription string // used by describe(ns); has a default
    DefaultLimit         int    // search results when the script gives no limit; default 8
    MaxLimit             int    // hard cap; default 25
    Warning              string // when non-empty, added as "warning" to search and describe results (partial catalogue)
}

type Catalog struct{ /* entries, index, byPath */ }
func New(entries []Entry, opts Options) *Catalog
func (c *Catalog) Entries() []Entry             // a fresh slice; Extra is copied, schemas are shared and must not be mutated
func (c *Catalog) Lookup(path string) (Entry, bool)
func (c *Catalog) Members() []Member            // namespace members, as declaration members, in entry order

type Hit struct {
    Path        string  `json:"path"`
    Kind        string  `json:"kind"`
    Description string  `json:"description"` // first sentence
    Score       float64 `json:"score"`       // rounded to 2 decimals
}
type SearchResult struct {
    Results   []Hit  `json:"results"`
    Total     int    `json:"total"`
    Truncated bool   `json:"truncated"`
    Warning   string `json:"warning,omitempty"`
    Hint      string `json:"hint,omitempty"` // on zero results: suggests describe(ns) and the groups
}
func (c *Catalog) Search(query string, limit int) SearchResult   // limit<=0 → DefaultLimit; capped at MaxLimit
// The namespace's own words (and "tools" while it is the alias) are ignored in
// queries, so search("rex") alone gives zero results plus the hint.

// Describe resolves a target, in this order:
//   "", ns                     → {path: ns, kind: "namespace", description, types: Declaration}
//   "tools"                    → the namespace when ns is "tools" or no entry path starts with "tools"; otherwise the group
//   an exact entry path        → {path, kind, description, types: "type Args = ...;\ntype Result = ...;"} ∪ Extra (copied)
//                                 (a namespace member also gets call: "await rex.capture({...})")
//   a group prefix ("api")     → {path, kind: "namespace", members: ["api.x: first sentence", ...] sorted, hint}
//   a bare member ("capture")  → the member (mcptool's wrapper also maps the tool's Name and "<ns>.<Name>")
//   unknown                    → error "describe: "x" not found. Did you mean: a, b?" — near matches on the last
//                                 path segment (prefix or small edit distance) first, then Search results
func (c *Catalog) Describe(target string) (map[string]any, error)
func (c *Catalog) Declaration() string          // declare const <ns>: { members }; a member whose identifier is already declared is skipped
func (c *Catalog) Hint() string                 // for codemode.Options.Hint
func (c *Catalog) Globals() map[string]codemode.HostFunc // "search" and "describe" taking positional JSON args
func GlobalsDeclaration() string                // declare function search(...) / describe(...) TypeScript

// Lower-level pieces, exported for hosts that render their own docs:
func Identifier(name string) string
func TypeOf(schema any, indent string) string
type Member struct { Name, Description string; Input, Output any }
func Signature(m Member, indent string) string
func Declaration(ns string, members []Member) string
func DocComment(text, indent string) string
type Document struct { Path, Text string }
type Match struct { Path string; Score float64 }
func Tokenize(text string) []string
type Index struct{...}; func NewIndex(docs []Document) *Index; func (idx *Index) Search(query string, limit int) []Match
```

`search` positional args from scripts: `[query]` or `[query, {limit}]`. Errors:
`search(query, {limit?}) needs a query string, e.g. await search("split a pane")`
and `search: query must be a non-empty string`. `describe` args: `[target]`;
error when missing names example targets.

The search text of an entry is `Path + Description + Keywords + property
names, descriptions and enum values from Input`.

## package mcptool

```go
type Mode string
const ( ModeOn Mode = "on"; ModeOnly Mode = "only"; ModeOff Mode = "off" )
func ParseMode(s string) (Mode, error)

// RunConfig is what Prepare returns for one invocation of the code tool.
type RunConfig struct {
    Context context.Context  // nil keeps the request context; tool calls run under it
    Entries []catalog.Entry  // extra catalogue entries for this run (API methods, ...)
    Args    func(tool string, args json.RawMessage) json.RawMessage // rewrite tool arguments; nil = unchanged
    Warning string           // added to search/describe results (partial catalogue)
    Cleanup func()           // called once the run has finished, whatever the outcome; not called when Prepare errors
}

type Options struct {
    Name           string            // code tool name; default "code"
    Namespace      string            // JS namespace; default "tools"
    StripPrefix    string            // removed from tool names to make member names ("rex_capture" → "capture")
    Mode           Mode              // default ModeOn
    Title          string            // code tool title; default "Run a script"
    Description    string            // code tool description; default generated (lists members, explains search/describe)
    Params         map[string]*jsonschema.Schema // extra properties on the code tool's input, validated (no defaults) and passed to Prepare; New panics if one cannot be resolved
    Prepare        func(ctx context.Context, args map[string]any) (*RunConfig, error) // optional per-run hook
    Catalog        catalog.Options   // Namespace is overridden
    Sandbox        codemode.Options  // Namespace and Hint are overridden; Timeout is the default per-call timeout, clamped to MaxTimeout
    MaxTimeout     time.Duration     // cap for the "timeout" argument; default 10m
    MaxResultChars int               // result JSON shown to the model; default 24 KiB
    TypesURI       string            // when set, Register also serves the declarations as a resource at this URI
}

type Registry struct{ /* server, opts, recorded tools */ }
func New(server *mcp.Server, opts Options) *Registry
// AddTool records a copy of a typed tool and, unless Mode is only, registers it on
// the server exactly as mcp.AddTool would. The copy's InputSchema (and OutputSchema
// when Out is not any) are derived when nil, so the catalogue and the server share
// one schema; the caller's struct is never written. In every mode it runs the SDK's
// own checks first and panics with the SDK's message where mcp.AddTool would, and it
// panics when the tool's name or member collides with another tool or the code tool.
func AddTool[In, Out any](r *Registry, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out])
// AddTool is the untyped form, mirroring (*mcp.Server).AddTool: no validation, no
// defaults, no panic on schemas the SDK accepts.
func (r *Registry) AddTool(t *mcp.Tool, h mcp.ToolHandler)
// Register adds the code tool (and the types resource); no-op in ModeOff. A tool
// added after Register re-registers the code tool so its description stays current.
func (r *Registry) Register()
func (r *Registry) Tools() []*mcp.Tool   // the registry's copies
// Call does what a script call does. The name is matched as given first (so dotted
// tool names work), then with "<ns>." stripped. The request carries no Session or
// Extra unless ctx comes from a code-tool run.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
func (r *Registry) Catalog(extra ...catalog.Entry) *catalog.Catalog
func (r *Registry) Declarations() string   // GlobalsDeclaration + namespace declaration
func (r *Registry) Guidance() string       // paragraph for server instructions, mode-aware
func UnwrapResult(res *mcp.CallToolResult, err error) (json.RawMessage, error)
```

Code tool input: `{code: string (required), timeout?: string}` plus `Params`.
`timeout` accepts `30s`, `2m`, or a number of seconds, clamped to
[1ns, MaxTimeout]; `0`, `"0"` and `"0s"` mean the default; NaN and ±Inf are
errors.

Script tool calls behave exactly like the same call over the wire. Typed
tools: arguments are validated against the resolved input schema (error
"invalid arguments: ..."), defaults applied, decoded case-sensitively into
`In`; the handler gets a `*mcp.CallToolRequest` with the name, the arguments
as sent, and the code-tool request's `Session` and `Extra`; the result is
shaped as the SDK's `applySchema` does (nil result → empty; typed-nil pointer
`Out` → the element's zero value; `Out` replaces StructuredContent and adds a
JSON text block when Content is empty or the output is not an object; `Out`
skipped when InputRequests is set); the output is validated against the
output schema with defaults ("validating tool output: ..."). Untyped tools
get no validation and no defaults; empty, `null` or `undefined` arguments
become `{}`. A handler panic rejects the call with `tool "X" panicked: ...`.
`UnwrapResult` then turns the result into the script's value: error →
reject; `IsError` → reject with the text; `StructuredContent` (JSON `null`
counts as absent) → its JSON; one text block that parses as a JSON object or
array → that JSON; else the text as a string, or the texts as a string
array; content with no text blocks → the JSON of the content array.

Result rendering (text content, plus structured content
`{ok, result, calls, elapsed_ms, truncated}` or `{ok:false, error, calls, logs}`),
`IsError` set on any failure. Results over MaxResultChars are clipped in the
text and `result` becomes the clipped text as a string; error text is clipped
too:

```
Script completed in 1.2s with 3 tool call(s).
<result>
{...clipped JSON...}
</result>
<console_output>
...
</console_output>
```
```
Script failed after 0.4s with 1 tool call(s):
Error: boom
    at <anonymous> (script.js:3)
```
```
Script timed out after 1.003s (limit 1s) with 2 tool call(s) started. Pass a longer timeout (max 10m), or split the work across scripts.
```
```
Script timed out after 302ms with 1 tool call(s) started: the request's deadline passed before the script's 1m0s timeout. A longer timeout will not help; split the work across scripts.
```

Modes: `on` → AddTool registers on the server and records, Register adds the
code tool. `only` → AddTool records only, Register adds the code tool. `off`
→ AddTool registers on the server, Register does nothing. A tool is valid or
invalid the same way in every mode.

## Testing

Every package has unit tests runnable offline with `go test ./...`. `mcptool`
tests use `mcp.NewInMemoryTransports` with a real `*mcp.Server` and
`*mcp.Client`. `gofmt -l .` must be empty and `go vet ./...` clean.

## fastschema/qjs facts the sandbox relies on

- `CloseOnContextDone` is baked into a process-wide compiled module on the
  first `qjs.New`. `Warm` passes `DisableBuildCache: true` once so the module
  is recompiled with the flag whatever ran before, then self-tests a busy
  loop against a 50ms deadline; if that fails, every `Run` returns an error.
- `MaxExecutionTime` is not enforced; the hard timeout is the ctx deadline
  closing the module. `Eval` and `rt.Close()` then panic and are recovered.
  wazero checks the ctx at function calls and loop back-edges, not inside a
  host call: `os.sleep` and libc timers (`WithSysNanosleep`) block until
  they end, hence the VM goroutine and the grace period in `Run`.
- The wasm build includes quickjs-libc (`qjs:std`, `qjs:os`, `qjs:bjson`,
  `js_std_add_helpers`); `Option.CWD` defaults to the process working
  directory mounted at "/" and `Stdout`/`Stderr` to the real ones, so the
  sandbox always sets all three.
- Values returned from `Eval`/`Invoke` are unreliable for short-lived
  strings; the result travels through the `__host_result` host function.
- `SetAsyncFunc` resolves synchronously; async host calls use the
  `__host_call` / `__host_wait` pump with the `__drive` loop in the prelude.
- Strings passed as arguments into Go `SetFunc` callbacks are reliable.
