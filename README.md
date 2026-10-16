# codemode-go

Code mode for Go: let an agent write one JavaScript program that calls your
Go functions, instead of one tool call per step.

The library follows the pattern that Cloudflare's
[`@cloudflare/codemode`](https://github.com/cloudflare/codemode) and pi's
[codemode extension](https://github.com/earendil-works/pi) established:

- one `code` tool takes a script; the script calls your functions as `await
  ns.name(args)` and combines the results in code;
- discovery is incremental and happens *inside* the script: `await
  search("what I need")` ranks the available functions, `await describe(path)`
  returns TypeScript declarations and the exact call to write;
- only the script's return value and console output reach the model, clipped
  to a size budget;
- independent calls run concurrently with `Promise.all`.

The sandbox is QuickJS compiled to WebAssembly, executed by
[wazero](https://wazero.io) through [fastschema/qjs](https://github.com/fastschema/qjs).
No cgo, no network, no filesystem, no timers, no modules. Every run is bounded
by a wall-clock timeout, a heap limit, a host-call budget, a concurrency cap
and a console cap.

```
github.com/thehumanworks/codemode-go           the sandbox (no other dependencies of yours)
github.com/thehumanworks/codemode-go/catalog   search() and describe() over a catalogue of functions
github.com/thehumanworks/codemode-go/mcptool   adapter for the official MCP Go SDK
```

Each package is usable on its own. The sandbox does not know about MCP;
`catalog` does not know about MCP either; only `mcptool` imports the SDK.

## Install

```sh
go get github.com/thehumanworks/codemode-go
```

## Sandbox: script your Go functions

A `Tool` is a name plus a function from JSON to JSON. `Run` executes a script
as the body of an async function, with the tools as methods of a namespace
object.

```go
tools := []codemode.Tool{
    {Name: "list_users", JSName: "listUsers", Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
        return json.RawMessage(`[{"name":"ada","admin":true},{"name":"bob","admin":false}]`), nil
    }},
    {Name: "greet", Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
        var in struct{ Name string }
        json.Unmarshal(args, &in)
        return json.Marshal("hello " + in.Name)
    }},
}

out, err := codemode.Run(ctx, `
    const users = await app.listUsers();
    const admins = users.filter(u => u.admin);
    console.log("admins:", admins.length);
    return Promise.all(admins.map(u => app.greet({name: u.name})));
`, tools, nil, codemode.Options{Namespace: "app"})

fmt.Println(string(out.Result)) // ["hello ada"]
fmt.Println(out.Logs)           // [admins: 1]
```

What the script can rely on:

| Behaviour | Detail |
|---|---|
| Calls | `await app.name(args)`; `args` is serialised with `JSON.stringify` and handed to the `HostFunc` |
| Results | the `HostFunc`'s JSON, parsed; an error rejects with its message, so `try/catch` works |
| Concurrency | `Promise.all` runs calls in parallel, up to `Options.MaxConcurrency` |
| Return value | serialised to `Output.Result`; `undefined` becomes `null` |
| Console | `console.log` and friends go to `Output.Logs` (and `Options.OnLog` live); kept even when the script times out |
| Unknown members | `TypeError: app.gret does not exist. Did you mean app.greet?` plus `Options.Hint` |
| Stalls | a script awaiting something nothing can settle fails fast instead of hanging |

Errors are typed: `*ScriptError` (thrown value, syntax error, stall, out of
memory, call limit), `*TimeoutError` (unwraps to `ErrTimeout`), and an error
wrapping `context.Canceled` when the caller cancels. All of them carry the
logs and the call count.

Globals are extra async functions taking positional arguments, passed to a
`HostFunc` as a JSON array; `catalog` uses them for `search` and `describe`.

Use a `Runner` when the tools are fixed, and call `codemode.Warm()` at start
up so the first script does not pay for compiling the WebAssembly module.

## Catalog: search and describe

`catalog` turns a list of entries, each with a path, a description and JSON
Schemas, into the two discovery globals and the TypeScript declarations.

```go
cat := catalog.New([]catalog.Entry{
    {Path: "app.listUsers", Description: "List every user.", Output: usersSchema},
    {Path: "app.greet", Description: "Greet a user by name.", Input: greetSchema},
    {Path: "api.billing.invoice", Kind: "api_method", Description: "Create an invoice.", Input: invoiceSchema,
        Extra: map[string]any{"call": `await app.call({method: "billing.invoice", args: {...}})`}},
}, catalog.Options{Namespace: "app"})

opts := codemode.Options{Namespace: "app", Hint: cat.Hint()}
out, err := codemode.Run(ctx, script, tools, cat.Globals(), opts)
```

Inside the script:

```js
const hits = await search("invoice a customer", {limit: 5});
// {results: [{path: "api.billing.invoice", kind: "api_method", description: "Create an invoice.", score: 4.1}], total: 1, truncated: false}
const sig = await describe("api.billing.invoice");
// {path, kind, description, types: "type Args = {...};\ntype Result = ...;", call: "await app.call({...})"}
const ns = await describe("app");        // declare const app: { ... } with every member
const group = await describe("api");     // every api.* entry, one line each
```

Search is BM25 over the path, description, keywords and the schema's property
names, descriptions and enum values, with camelCase splitting and light
stemming. Describe renders JSON Schema as TypeScript: optional properties,
JSDoc from descriptions and defaults, enums as literal unions, arrays, tuples,
`Record<>`, nullable unions, `anyOf`/`oneOf`/`allOf`, and local `$ref`s with
a depth guard. The lower-level pieces (`TypeOf`, `Declaration`, `NewIndex`)
are exported for hosts that render their own documentation.

## MCP: one code tool for your server

`mcptool` wires everything to a server built on
[`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
Register tools through the `Registry` instead of directly on the server:

```go
srv := mcp.NewServer(&mcp.Implementation{Name: "notes", Version: "1.0"}, nil)
reg := mcptool.New(srv, mcptool.Options{
    Namespace:   "notes",
    StripPrefix: "notes_",       // notes_add is called as notes.add
    Mode:        mcptool.ModeOn, // on | only | off
    TypesURI:    "notes://code/types",
})

mcptool.AddTool(reg, &mcp.Tool{Name: "notes_add", Description: "Store a note."}, addHandler)
mcptool.AddTool(reg, &mcp.Tool{Name: "notes_list", Description: "List note titles."}, listHandler)
reg.Register() // adds the "code" tool; call it after every AddTool
go codemode.Warm()
```

`AddTool` has the same signature as `mcp.AddTool`, derives the input schema
from the handler's input type when the tool gives none, and registers the tool
on the server exactly as the SDK would. When a script calls a tool, the
arguments are validated against that schema and defaults applied, so the
script sees what a client over the wire would see. Results are unwrapped the
way Cloudflare's codemode does: structured content wins, a JSON text block
becomes its value, other text becomes a string, `isError` rejects.

The model gets a `code` tool with `code` and `timeout` arguments, a generated
description that lists the members and explains `search` and `describe`, and
results like:

```
Script completed in 212ms with 3 tool call(s).
<result>
{"stored":3,"longest":"groceries"}
</result>
<console_output>
titles: groceries, ideas, todo
</console_output>
```

Failures say `Script failed after 40ms with 1 tool call(s):` followed by the
JavaScript error and stack, or `Script timed out after 1m0s ...`. Structured
content carries `{ok, result, calls, elapsed_ms, truncated, logs}` for clients
that read it.

Modes:

| Mode | Tool list | Scripts |
|---|---|---|
| `on` | every tool plus `code` | can call every tool |
| `only` | just `code`; the tool list costs one schema | can call every tool |
| `off` | every tool, no `code` | n/a |

`Registry.Guidance()` returns a paragraph for the server's instructions that
tells the model when to prefer the code tool.

Hosts with dynamic catalogues (methods discovered at runtime, per-call
settings) use `Options.Params` to add properties to the code tool's input and
`Options.Prepare` to turn them into a `RunConfig`: a context for the tool
calls, extra catalogue entries, a warning, and an argument rewriter.
[rex-mcp-server](https://github.com/thehumanworks/rex-mcp-server) does this to
expose a terminal multiplexer's whole API through one tool.

See `examples/mcpserver` for a complete stdio server and `examples/basic` for
the sandbox without MCP.

## Limits and safety

| Limit | Default | Option |
|---|---|---|
| Wall clock | 60s | `Options.Timeout`, or a context deadline |
| Heap | 64 MiB | `Options.MemoryLimit` |
| Host calls per script | 200 | `Options.MaxCalls` |
| Calls in flight | 8 | `Options.MaxConcurrency` |
| Console | 64 KiB | `Options.MaxLogChars` |
| Result shown to the model (mcptool) | 24 KiB | `mcptool.Options.MaxResultChars` |

The script runs in a fresh VM each time. The host bridge is installed before
the script and removed from `globalThis` by the prelude, so the only way out
is through the namespace methods and the globals you provide. The timeout is
enforced by closing the WebAssembly module, which stops a tight loop as well
as a stuck host call; in-flight host calls see their context cancelled.

A script is untrusted input: it can only do what your `HostFunc`s do, so put
authorisation and rate limits in them, not in the prompt.

## Notes on fastschema/qjs

A few facts about the underlying library shaped the implementation and are
worth knowing if you touch `sandbox.go`:

- The timeout flag (`CloseOnContextDone`) is baked into a process-wide
  compiled module on the first runtime creation. Every runtime in this package
  passes it; a runtime created elsewhere without it would silently disable
  timeouts for later scripts.
- Values returned from `Eval` are unreliable for short-lived strings, so the
  result travels through a global the prelude sets.
- Async host functions resolve synchronously, so host calls use a
  start/wait pump driven by a loop in the prelude that yields to the microtask
  queue between results.

## Development

```sh
go test ./...          # unit tests for every package, offline
go test -race ./...
go vet ./... && gofmt -l .
```

`DESIGN.md` holds the API contract the packages are built to.
