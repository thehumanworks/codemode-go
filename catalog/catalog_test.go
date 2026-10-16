package catalog

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// structSchema mimics a typed schema value such as *jsonschema.Schema.
type structSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
	Required   []string       `json:"required,omitempty"`
}

func testEntries(t *testing.T) []Entry {
	t.Helper()
	return []Entry{
		{
			Path:        "rex.capture",
			Kind:        "tool",
			Description: "Read a pane's screen. Returns the current screen contents of a terminal pane plus process state.",
			Keywords:    "rex_capture",
			Input: schema(t, `{
				"type":"object",
				"properties":{
					"block":{"type":"string","description":"Pane id to read"},
					"lines":{"type":"integer","description":"Scrollback lines","minimum":1},
					"mode":{"type":"string","enum":["screen","scrollback"]}
				},
				"required":["block"]
			}`),
		},
		{
			Path:        "rex.send",
			Description: "Type into a pane. Sends text and key presses to a terminal pane.",
			Input: schema(t, `{
				"type":"object",
				"properties":{
					"text":{"type":"string","description":"Text to type"},
					"keys":{"type":"array","items":{"type":"string"},"description":"Key presses such as Enter"}
				},
				"required":["text"]
			}`),
			Extra: map[string]any{"call": `await rex.send({text: "ls\n"})`, "examples": []any{"rex.send({text: \"q\"})"}},
		},
		{
			Path:        "rex.api",
			Kind:        "tool",
			Description: "Call a server API method.",
			Input:       schema(t, `{"type":"object","properties":{"method":{"type":"string"}}}`),
		},
		{
			Path:        "api.session.view",
			Kind:        "api_method",
			Description: "Describe the session layout. Lists windows, tabs and panes.",
			Input:       schema(t, `{"type":"object","properties":{"session":{"type":"string"}}}`),
			Output:      schema(t, `{"type":"object","properties":{"windows":{"type":"array","items":{"type":"string"}}},"required":["windows"]}`),
			Extra: map[string]any{
				"call":            `await rex.api({op: "call", method: "session.view", args: {...}})`,
				"request_example": map[string]any{"session": "main"},
				"path":            "should not override",
				"description":     "should not override",
			},
		},
		{
			Path:        "api.session.new_split",
			Kind:        "api_method",
			Description: "Split a pane: create a new block beside an existing one.",
			Input:       schema(t, `{"type":"object","properties":{"direction":{"type":"string","enum":["left","right","up","down"]}}}`),
		},
		{
			Path:        "block.pwd",
			Kind:        "block_method",
			Description: "Working directory of a shell pane (block method of shell panes)",
			Output:      schema(t, `{"type":"object","properties":{"cwd":{"type":"string"}}}`),
			Extra:       map[string]any{"block_type": "shell"},
		},
	}
}

func testCatalog(t *testing.T, opts Options) *Catalog {
	t.Helper()
	if opts.Namespace == "" {
		opts.Namespace = "rex"
	}
	return New(testEntries(t), opts)
}

func TestCatalogSearch(t *testing.T) {
	c := testCatalog(t, Options{})
	res := c.Search("read screen of pane", 0)
	if len(res.Results) == 0 || res.Results[0].Path != "rex.capture" {
		t.Fatalf("screen: %+v", res.Results)
	}
	top := res.Results[0]
	if top.Kind != "tool" || top.Description != "Read a pane's screen." {
		t.Errorf("hit: %+v", top)
	}
	if top.Score <= 0 || math.Abs(top.Score*100-math.Round(top.Score*100)) > 1e-9 {
		t.Errorf("score not rounded to 2 decimals: %v", top.Score)
	}
	if got := c.Search("split", 0).Results; len(got) == 0 || got[0].Path != "api.session.new_split" {
		t.Errorf("split: %+v", got)
	}
	// Enum values and property descriptions are searchable.
	if got := c.Search("scrollback", 0).Results; len(got) == 0 || got[0].Path != "rex.capture" {
		t.Errorf("enum: %+v", got)
	}
	if got := c.Search("rex_capture", 0).Results; len(got) == 0 || got[0].Path != "rex.capture" {
		t.Errorf("keywords: %+v", got)
	}

	all := c.Search("pane", 0)
	if all.Total < 3 || all.Truncated || len(all.Results) != all.Total {
		t.Fatalf("pane: total %d truncated %v results %d", all.Total, all.Truncated, len(all.Results))
	}
	one := c.Search("pane", 1)
	if len(one.Results) != 1 || one.Total != all.Total || !one.Truncated {
		t.Errorf("limit 1: %+v", one)
	}
	if one.Hint != "" || one.Warning != "" {
		t.Errorf("unexpected hint or warning: %+v", one)
	}

	capped := testCatalog(t, Options{DefaultLimit: 1, MaxLimit: 2})
	if got := capped.Search("pane", 0); len(got.Results) != 1 || !got.Truncated {
		t.Errorf("default limit: %+v", got)
	}
	if got := capped.Search("pane", 100); len(got.Results) != 2 || got.Total != all.Total {
		t.Errorf("max limit: %+v", got)
	}
}

func TestCatalogSearchNoMatch(t *testing.T) {
	c := testCatalog(t, Options{})
	res := c.Search("zebra unicorn", 0)
	if res.Total != 0 || res.Truncated || res.Results == nil {
		t.Errorf("no match: %+v", res)
	}
	want := `No match. Try other words, or describe("rex") to list every tool, describe("api") for every api method, describe("block") for every block method.`
	if res.Hint != want {
		t.Errorf("hint:\n got %s\nwant %s", res.Hint, want)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"results":[]`) {
		t.Errorf("results should marshal as []: %s", b)
	}
	if strings.Contains(string(b), "warning") {
		t.Errorf("empty warning should be omitted: %s", b)
	}
	if got := c.Search("", 0); got.Total != 0 || got.Results == nil {
		t.Errorf("empty query: %+v", got)
	}
}

func TestCatalogWarning(t *testing.T) {
	const w = "server API methods are not searchable right now: offline"
	c := testCatalog(t, Options{Warning: w})
	if got := c.Search("pane", 0).Warning; got != w {
		t.Errorf("search warning: %q", got)
	}
	for _, target := range []string{"rex", "api", "rex.capture", "api.session.view"} {
		out, err := c.Describe(target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if out["warning"] != w {
			t.Errorf("%s: warning %v", target, out["warning"])
		}
	}
	_, err := c.Describe("zebra")
	if err == nil || !strings.HasSuffix(err.Error(), " ("+w+")") {
		t.Errorf("not found warning: %v", err)
	}
	if out, _ := testCatalog(t, Options{}).Describe("rex"); out["warning"] != nil {
		t.Errorf("no warning expected: %v", out["warning"])
	}
}

func TestDescribeNamespace(t *testing.T) {
	c := testCatalog(t, Options{NamespaceDescription: "Rex tools."})
	for _, target := range []string{"", "rex", "tools", " rex "} {
		out, err := c.Describe(target)
		if err != nil {
			t.Fatalf("%q: %v", target, err)
		}
		types, _ := out["types"].(string)
		if out["path"] != "rex" || out["kind"] != "namespace" || out["description"] != "Rex tools." {
			t.Errorf("%q: %v", target, out)
		}
		for _, want := range []string{"declare const rex: {", "capture(args: {", "block: string;", "send(args: {", "api(args: {"} {
			if !strings.Contains(types, want) {
				t.Errorf("%q: missing %q in\n%s", target, want, types)
			}
		}
		if strings.Contains(types, "session") || strings.Contains(types, "pwd") {
			t.Errorf("%q: non-members in declaration:\n%s", target, types)
		}
	}
	out, _ := testCatalog(t, Options{}).Describe("")
	if out["description"] != defaultNamespaceDescription {
		t.Errorf("default description: %v", out["description"])
	}
}

func TestDescribeGroup(t *testing.T) {
	c := testCatalog(t, Options{})
	out, err := c.Describe("api")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"api.session.new_split: Split a pane: create a new block beside an existing one.",
		"api.session.view: Describe the session layout.",
	}
	if out["path"] != "api" || out["kind"] != "namespace" || !reflect.DeepEqual(out["members"], want) {
		t.Errorf("api: %v", out)
	}
	if hint, _ := out["hint"].(string); !strings.Contains(hint, `describe("api.<name>")`) {
		t.Errorf("hint: %q", hint)
	}
	// Nested prefixes are groups too.
	out, err = c.Describe("api.session")
	if err != nil || len(out["members"].([]string)) != 2 {
		t.Errorf("api.session: %v %v", out, err)
	}
	out, err = c.Describe("block")
	if err != nil || !reflect.DeepEqual(out["members"], []string{"block.pwd: Working directory of a shell pane (block method of shell panes)"}) {
		t.Errorf("block: %v %v", out, err)
	}
}

func TestDescribeMember(t *testing.T) {
	c := testCatalog(t, Options{})
	for _, target := range []string{"rex.capture", "capture"} {
		out, err := c.Describe(target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if out["path"] != "rex.capture" || out["kind"] != "tool" || out["call"] != "await rex.capture({...})" {
			t.Errorf("%s: %v", target, out)
		}
		if !strings.HasPrefix(out["description"].(string), "Read a pane's screen. Returns") {
			t.Errorf("%s: description %v", target, out["description"])
		}
		types := out["types"].(string)
		for _, want := range []string{"declare const rex: {", "capture(args: {", "/** Pane id to read */", "block: string;", `mode?: "screen" | "scrollback";`} {
			if !strings.Contains(types, want) {
				t.Errorf("%s: missing %q in\n%s", target, want, types)
			}
		}
		if strings.Contains(types, "send(") {
			t.Errorf("%s: other members in types:\n%s", target, types)
		}
	}

	// Members default to kind "tool"; Extra may override call and adds keys.
	out, err := c.Describe("send")
	if err != nil {
		t.Fatal(err)
	}
	if out["kind"] != "tool" || out["call"] != `await rex.send({text: "ls\n"})` || out["examples"] == nil {
		t.Errorf("send: %v", out)
	}

	// A member named like a group: the bare name is the group, the full path the member.
	if out, _ := c.Describe("api"); out["kind"] != "namespace" {
		t.Errorf("api should be the group: %v", out)
	}
	if out, _ := c.Describe("rex.api"); out["kind"] != "tool" || out["call"] != "await rex.api({...})" {
		t.Errorf("rex.api should be the member: %v", out)
	}
}

func TestDescribeEntry(t *testing.T) {
	c := testCatalog(t, Options{})
	out, err := c.Describe("api.session.view")
	if err != nil {
		t.Fatal(err)
	}
	if out["path"] != "api.session.view" || out["kind"] != "api_method" || out["description"] != "Describe the session layout. Lists windows, tabs and panes." {
		t.Errorf("Extra overrode a fixed key: %v", out)
	}
	if out["call"] != `await rex.api({op: "call", method: "session.view", args: {...}})` {
		t.Errorf("call: %v", out["call"])
	}
	if !reflect.DeepEqual(out["request_example"], map[string]any{"session": "main"}) {
		t.Errorf("extra: %v", out["request_example"])
	}
	wantTypes := "type Args = {\n  session?: string;\n};\ntype Result = {\n  windows: string[];\n};"
	if out["types"] != wantTypes {
		t.Errorf("types:\n%s\nwant\n%s", out["types"], wantTypes)
	}

	out, err = c.Describe("block.pwd")
	if err != nil {
		t.Fatal(err)
	}
	if out["types"] != "type Result = {\n  cwd?: string;\n};" || out["block_type"] != "shell" || out["call"] != nil {
		t.Errorf("block.pwd: %v", out)
	}

	// Bare names only resolve namespace members.
	if _, err := c.Describe("pwd"); err == nil {
		t.Error("bare non-member name should not resolve")
	}
}

func TestDescribeNotFound(t *testing.T) {
	c := testCatalog(t, Options{})
	_, err := c.Describe("screen capture")
	if err == nil || !strings.HasPrefix(err.Error(), `describe: "screen capture" not found. Did you mean: rex.capture`) {
		t.Errorf("did you mean: %v", err)
	}
	_, err = c.Describe("zebra")
	want := `describe: "zebra" not found. Use search("...") or describe("rex"), describe("api"), describe("block").`
	if err == nil || err.Error() != want {
		t.Errorf("no suggestion:\n got %v\nwant %s", err, want)
	}
}

func TestGlobals(t *testing.T) {
	c := testCatalog(t, Options{})
	g := c.Globals()
	if len(g) != 2 || g["search"] == nil || g["describe"] == nil {
		t.Fatalf("globals: %v", g)
	}
	ctx := context.Background()
	call := func(name, args string) (string, error) {
		out, err := g[name](ctx, json.RawMessage(args))
		return string(out), err
	}

	out, err := call("search", `["pane", {"limit": 1}]`)
	if err != nil {
		t.Fatal(err)
	}
	var res SearchResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || !res.Truncated || res.Total < 3 {
		t.Errorf("search: %s", out)
	}
	if out, err := call("search", `["read screen"]`); err != nil || !strings.Contains(out, `"path":"rex.capture"`) {
		t.Errorf("search without options: %s %v", out, err)
	}
	if out, err := call("search", `["pane", 2]`); err != nil || strings.Count(out, `"path"`) != 2 {
		t.Errorf("numeric limit: %s %v", out, err)
	}

	usage := `search(query, {limit?}) needs a query string, e.g. await search("split a pane")`
	for _, args := range []string{`[]`, `{}`, `not json`, `null`} {
		if _, err := call("search", args); err == nil || err.Error() != usage {
			t.Errorf("search %s: %v", args, err)
		}
	}
	for _, args := range []string{`[""]`, `["   "]`, `[5]`, `[null]`} {
		if _, err := call("search", args); err == nil || err.Error() != "search: query must be a non-empty string" {
			t.Errorf("search %s: %v", args, err)
		}
	}

	out, err = call("describe", `["rex.capture"]`)
	if err != nil {
		t.Fatal(err)
	}
	var desc map[string]any
	if err := json.Unmarshal([]byte(out), &desc); err != nil || desc["path"] != "rex.capture" || desc["call"] != "await rex.capture({...})" {
		t.Errorf("describe: %s %v", out, err)
	}
	wantUsage := `describe(target) needs a path such as "rex.capture", "api.session.view", "block.pwd", or a namespace: "rex", "api", "block"`
	for _, args := range []string{`[]`, `not json`} {
		if _, err := call("describe", args); err == nil || err.Error() != wantUsage {
			t.Errorf("describe %s:\n got %v\nwant %s", args, err, wantUsage)
		}
	}
	if _, err := call("describe", `[5]`); err == nil || err.Error() != "describe: target must be a string" {
		t.Errorf("describe number: %v", err)
	}
	if _, err := call("describe", `["zebra"]`); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("describe unknown: %v", err)
	}

	empty := New(nil, Options{Namespace: "x"})
	if _, err := empty.Globals()["describe"](ctx, json.RawMessage(`[]`)); err == nil || err.Error() != `describe(target) needs a path or a namespace: "x"` {
		t.Errorf("empty usage: %v", err)
	}
}

func TestCatalogDeclarations(t *testing.T) {
	c := testCatalog(t, Options{})
	if d := c.Declaration(); !strings.HasPrefix(d, "declare const rex: {") || !strings.Contains(d, "/** Call a server API method. */") {
		t.Errorf("declaration:\n%s", d)
	}
	h := c.Hint()
	if !strings.Contains(h, "search(") || !strings.Contains(h, `describe("rex.<name>")`) {
		t.Errorf("hint: %s", h)
	}
	gd := GlobalsDeclaration()
	for _, want := range []string{"declare function search(query: string, opts?: { limit?: number })", "declare function describe(path: string)"} {
		if !strings.Contains(gd, want) {
			t.Errorf("globals declaration missing %q:\n%s", want, gd)
		}
	}
}

func TestCatalogAccessors(t *testing.T) {
	entries := append(testEntries(t),
		Entry{Path: "rex.capture", Description: "duplicate, dropped"},
		Entry{Path: "", Description: "no path, dropped"},
		Entry{Path: "rex.sub.deep", Description: "under the namespace but not a member"},
	)
	c := New(entries, Options{Namespace: "rex"})
	if got := len(c.Entries()); got != len(testEntries(t))+1 {
		t.Errorf("entries: %d", got)
	}
	e, ok := c.Lookup("rex.capture")
	if !ok || !strings.HasPrefix(e.Description, "Read a pane") {
		t.Errorf("lookup: %+v %v", e, ok)
	}
	if _, ok := c.Lookup("capture"); ok {
		t.Error("lookup is by exact path")
	}
	var names []string
	for _, m := range c.Members() {
		names = append(names, m.Name)
	}
	if !reflect.DeepEqual(names, []string{"capture", "send", "api"}) {
		t.Errorf("members: %v", names)
	}
	// "rex.sub" is a group under the namespace; "rex" is not listed as a group.
	if out, err := c.Describe("rex.sub"); err != nil || out["kind"] != "namespace" {
		t.Errorf("rex.sub: %v %v", out, err)
	}
	if h := c.Search("zebra", 0).Hint; strings.Count(h, `describe("rex")`) != 1 {
		t.Errorf("hint: %s", h)
	}
	// Defaults.
	d := New(nil, Options{})
	if out, _ := d.Describe(""); out["path"] != "tools" {
		t.Errorf("default namespace: %v", out["path"])
	}
}

func TestSchemaNormalisation(t *testing.T) {
	in := &structSchema{
		Type: "object",
		Properties: map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob to match"},
			"limit":   map[string]any{"type": "integer"},
		},
		Required: []string{"pattern"},
	}
	out := structSchema{Type: "object", Properties: map[string]any{"files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}
	c := New([]Entry{
		{Path: "fs.find", Kind: "tool", Description: "Find files.", Input: in, Output: out},
		{Path: "fs.raw", Description: "Raw JSON schema.", Input: json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}}}`)},
		{Path: "fs.none", Description: "Typed nil schema.", Input: (*structSchema)(nil)},
		{Path: "fs.bad", Description: "Unmarshalable schema.", Input: func() {}},
	}, Options{Namespace: "fs"})

	e, _ := c.Lookup("fs.find")
	if _, ok := e.Input.(map[string]any); !ok {
		t.Fatalf("input not normalised: %T", e.Input)
	}
	desc, err := c.Describe("find")
	if err != nil {
		t.Fatal(err)
	}
	types := desc["types"].(string)
	for _, want := range []string{"find(args: {", "/** Glob to match */", "pattern: string;", "limit?: number;", "files?: string[];"} {
		if !strings.Contains(types, want) {
			t.Errorf("missing %q in\n%s", want, types)
		}
	}
	if got := c.Search("glob", 0).Results; len(got) == 0 || got[0].Path != "fs.find" {
		t.Errorf("schema words not indexed: %+v", got)
	}
	if desc, _ := c.Describe("raw"); !strings.Contains(desc["types"].(string), "x?: number;") {
		t.Errorf("raw: %v", desc["types"])
	}
	for _, name := range []string{"fs.none", "fs.bad"} {
		e, _ := c.Lookup(name)
		if e.Input != nil {
			t.Errorf("%s: input should be nil, got %#v", name, e.Input)
		}
		desc, _ := c.Describe(name)
		if !strings.Contains(desc["types"].(string), "(): Promise<unknown>;") {
			t.Errorf("%s: types %v", name, desc["types"])
		}
		if desc["call"] != "await fs."+strings.TrimPrefix(name, "fs.")+"()" {
			t.Errorf("%s: call %v", name, desc["call"])
		}
	}
}

func TestFirstSentence(t *testing.T) {
	cases := map[string]string{
		"Read a pane. More text.":       "Read a pane.",
		"Read a pane.\nMore text.":      "Read a pane.",
		"One line only":                 "One line only",
		"Wrapped\nsentence. Next.":      "Wrapped sentence.",
		"Title line\n\nLong body here.": "Title line",
		"  v1.2 is out. Upgrade.  ":     "v1.2 is out.",
		"":                              "",
	}
	for in, want := range cases {
		if got := firstSentence(in); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}
