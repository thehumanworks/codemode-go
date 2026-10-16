// Package catalog lets sandboxed scripts discover what they can call,
// incrementally, instead of receiving every type up front.
//
// A Catalog holds entries: the members of the sandbox namespace (tools) plus
// anything else worth finding, such as API methods reached through a generic
// tool. Each entry has a dotted path, a description, and optional JSON
// Schemas for its argument and result. The catalogue builds a BM25 index over
// them and renders schemas as TypeScript, then exposes two host globals for
// the codemode sandbox:
//
//	search(query, {limit?})  ranked paths with a one-sentence description
//	describe(path)           TypeScript types, how to call it, and extras
//
// describe also accepts the namespace (the declaration of every member) and
// any group prefix such as "api" (a listing of the entries under it).
//
// Usage:
//
//	cat := catalog.New([]catalog.Entry{
//		{Path: "tools.capture", Kind: "tool", Description: "Read a pane's screen.", Input: captureSchema},
//		{Path: "api.session.view", Kind: "api_method", Description: "Describe the session.",
//			Extra: map[string]any{"call": `await tools.api({method: "session.view"})`}},
//	}, catalog.Options{Namespace: "tools"})
//
//	out, err := codemode.Run(ctx, code, tools, cat.Globals(), codemode.Options{
//		Namespace: "tools",
//		Hint:      cat.Hint(),
//	})
//
// catalog.GlobalsDeclaration() + "\n\n" + cat.Declaration() is the full
// TypeScript a host can show to the model or serve as a resource.
//
// Schemas are accepted as any value that marshals to JSON Schema (a
// map[string]any, a *jsonschema.Schema, a struct, raw JSON) and are decoded
// once, when the Catalog is built. A Catalog is immutable and safe for
// concurrent use.
package catalog
