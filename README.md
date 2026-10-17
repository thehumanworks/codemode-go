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
No cgo. The library ships its own QuickJS build without quickjs-libc, so
the VM has no files, timers, environment, network, process access or module
loader: a script can only compute and call the host functions it was given.
Every run is bounded by a wall-clock timeout, a heap limit, a host-call
budget, a concurrency cap and a console cap.

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
| Console | `console.log` and friends go to `Output.Logs` (and `Options.OnLog` live); kept even when the script times out; once `MaxLogChars` is reached, later lines are dropped after a marker |
| Unknown members | `TypeError: app.gret does not exist. Did you mean app.greet?` plus `Options.Hint` |
| Stalls | a script awaiting something nothing can settle fails fast instead of hanging |

Errors are typed: `*ScriptError` (thrown value, syntax error, stall, out of
memory, call limit, a tool that panicked), `*TimeoutError` (unwraps to
`ErrTimeout`; its `Timeout` is the limit that applied, the option or the
shorter context deadline), and an error wrapping `context.Canceled` when the
caller cancels. All of them carry the logs and the call count. Stack traces
use the script's own line numbers.

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
names, descriptions and enum values, with camelCase and acronym splitting,
Unicode words and light stemming; the namespace's own name is ignored in
queries. Unknown targets get did-you-mean suggestions, nearest member first.
Describe renders JSON Schema as TypeScript: optional properties, JSDoc from
descriptions, defaults and constraints, enums as literal unions, arrays,
tuples, `Record<>`, nullable unions, `anyOf`/`oneOf`/`allOf` narrowing the
base schema, and local `$ref`s with cycles cut. Schemas can be maps,
`*jsonschema.Schema` values or raw JSON. The lower-level pieces (`TypeOf`, `Declaration`, `NewIndex`)
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
reg.Register() // adds the "code" tool (tools added later re-register it)
go codemode.Warm()
```

`AddTool` has the same signature as `mcp.AddTool`, derives the input schema
from the handler's input type when the tool gives none, and registers a copy
of the tool on the server exactly as the SDK would. A script call behaves
like the same call over the wire: arguments are validated and defaulted,
output is validated, and the handler sees the code tool's session. A panic
in a handler becomes an error result, whether the call came from a script or
directly from a client (the SDK alone would end the process). A parity test
compares every case through a real client. Name collisions (two tools that
would become the same member, or a tool named like the code tool) panic at
registration. Results are unwrapped the way Cloudflare's codemode does:
structured content wins, a JSON text block becomes its value, other text
becomes a string, `isError` rejects.

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
JavaScript error and stack, or `Script timed out after 1m0.01s (limit 1m0s)
...` (a timeout caused by the request's own deadline says so instead of
suggesting a longer `timeout`). Results and errors are clipped to
`MaxResultChars` in both the text and the structured content, which carries
`{ok, result, calls, elapsed_ms, truncated, logs}` for clients that read it.

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
calls, extra catalogue entries, a warning, an argument rewriter and a cleanup
hook. `Params` values are validated against their schemas before `Prepare`
runs.
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
is through the namespace methods and the globals you provide; the host does
not trust the prelude alone and validates the JSON that crosses the bridge
in both directions. Calls over the budget, unknown names and bad arguments
are rejected synchronously, so a burst of fire-and-forget calls cannot wedge
a run. A `HostFunc` that panics rejects the call instead of ending the
process. The timeout is enforced by closing the WebAssembly module, which
stops a tight loop as well as a stuck host call; in-flight host calls see
their context cancelled and get one second to return.

The embedded `wasm/qjs.wasm` is QuickJS-ng compiled without quickjs-libc
([wasm/README.md](wasm/README.md) has the pinned sources and the build
script). The `qjs:std`, `qjs:os` and `qjs:bjson` modules, `print`,
`scriptArgs`, timers, `std.exit` and the file-backed module loader do not
exist in it, so `import()` of anything throws a `ReferenceError`. The VM
also runs with an empty read-only directory as its filesystem root, stdout
and stderr sent to `io.Discard`, no environment and no network, which only
matters if a stock qjs module were ever substituted. As a result nothing
inside the VM can block: the deadline interrupts a script at its next loop
iteration or call, and the one-second grace in `Run` is a backstop that is
not expected to be reached.

A script is untrusted input: it can only do what your `HostFunc`s do, so put
authorisation and rate limits in them, not in the prompt.

## Notes on fastschema/qjs

A few facts about the underlying library shaped the implementation and are
worth knowing if you touch `sandbox.go`:

- The timeout flag (`CloseOnContextDone`) is baked into a process-wide
  compiled module on the first runtime creation. `Warm` (which `Run` calls)
  forces one recompilation with the flag set and then checks that a busy
  loop stops on its deadline; if a runtime created elsewhere has disabled
  that, every `Run` returns an error rather than running scripts unbounded.
- The default options mount the process working directory as the VM's
  filesystem and connect its stdout and stderr to the real ones; the sandbox
  overrides all three.
- Values returned from `Eval` are unreliable for short-lived strings, so the
  result travels through a host function the prelude's driver calls.
- Async host functions resolve synchronously, so host calls use a
  start/wait pump driven by a loop in the prelude that yields to the microtask
  queue between results.

## Development

`mise.toml` declares the toolchain: `mise install` fetches the Go version
go.mod asks for and the tools that rebuild the QuickJS module, then

```sh
mise run test   # go test -race ./... for every package, offline
mise run lint   # gofmt and go vet
mise run wasm   # rebuild wasm/qjs.wasm, see wasm/README.md
```

Plain `go test ./...` works with any Go matching go.mod; the committed
module needs no build tools. `flake.nix` offers the same through Nix
(`nix develop`, `nix build`, `nix build .#qjs-wasm`); it was written without
a Nix installation to test it on, so expect to fill in the `vendorHash` it
asks for on the first build.

`DESIGN.md` holds the API contract the packages are built to.
`wasm/README.md` documents the embedded QuickJS build and how to rebuild it.

## License

MIT, see [LICENSE](LICENSE). The QuickJS glue under `wasm/glue` is from
[fastschema/qjs](https://github.com/fastschema/qjs) (MIT, `wasm/glue/LICENSE`);
QuickJS-ng is MIT as well.
