package catalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func schema(t *testing.T, src string) Schema {
	t.Helper()
	var s Schema
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTypeOf(t *testing.T) {
	s := schema(t, `{
		"type":"object",
		"properties":{
			"what":{"type":"string","enum":["a","b"],"description":"What to inspect","default":"a"},
			"n":{"type":"integer","minimum":1},
			"ids":{"type":"array","items":{"type":"string"}},
			"flag":{"type":"boolean"},
			"maybe":{"type":["string","null"]},
			"nested":{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]},
			"map":{"type":"object","additionalProperties":{"type":"integer"}},
			"any":{},
			"ref":{"$ref":"#/$defs/Thing"},
			"weird-name":{"type":"string"}
		},
		"required":["what","ids"],
		"$defs":{"Thing":{"type":"object","properties":{"id":{"type":"string"}}}}
	}`)
	got := TypeOf(s, "")
	for _, want := range []string{
		`/** What to inspect @default "a" */`,
		`what: "a" | "b";`,
		`/** @minimum 1 */`,
		`n?: number;`,
		`ids: string[];`,
		`flag?: boolean;`,
		`maybe?: string | null;`,
		"nested?: {\n    x: number;\n  };",
		`map?: Record<string, number>;`,
		`any?: unknown;`,
		"ref?: {\n    id?: string;\n  };",
		`"weird-name"?: string;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestTypeOfUnionsAndTuples(t *testing.T) {
	cases := map[string]string{
		`{"anyOf":[{"type":"string"},{"type":"integer"}]}`:                       "string | number",
		`{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"null"}]}}`: "(string | null)[]",
		`{"type":"array","items":[{"type":"string"},{"type":"number"}]}`:         "[string, number]",
		`{"const":5}`:     "5",
		`{"type":"null"}`: "null",
		`true`:            "unknown",
		`{"type":"object","additionalProperties":false}`: "Record<string, never>",
	}
	for src, want := range cases {
		var v any
		_ = json.Unmarshal([]byte(src), &v)
		if got := TypeOf(v, ""); got != want {
			t.Errorf("%s: got %q want %q", src, got, want)
		}
	}
}

func TestDeclaration(t *testing.T) {
	in := schema(t, `{"type":"object","properties":{"block":{"type":"string"}}}`)
	out := schema(t, `{"type":"object","properties":{"screen":{"type":"string"}},"required":["screen"]}`)
	d := Declaration("rex", []Member{
		{Name: "capture", Description: "Read a pane.\nSecond line.", Input: in, Output: out},
		{Name: "whoami", Description: "Who am I"},
	})
	for _, want := range []string{
		"declare const rex: {",
		"  /**\n   * Read a pane.\n   * Second line.\n   */",
		"  capture(args: {\n    block?: string;\n  }): Promise<{\n    screen: string;\n  }>;",
		"  /** Who am I */\n  whoami(): Promise<unknown>;",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in:\n%s", want, d)
		}
	}
	if Identifier("mcp__dev-radius__search") != "mcp__dev_radius__search" || Identifier("9x") != "_x" {
		t.Error("identifier")
	}
}

func TestRecursiveSchemaTerminates(t *testing.T) {
	s := schema(t, `{"$defs":{"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}},"$ref":"#/$defs/Node"}`)
	got := TypeOf(s, "")
	if !strings.Contains(got, "unknown") || len(got) > 2000 {
		t.Errorf("recursion not bounded: %d chars", len(got))
	}
}

func typeOfJSON(t *testing.T, src string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return TypeOf(v, "")
}

// Array items that render as a union or intersection need parentheses;
// items that merely contain "|" inside braces or angle brackets do not.
func TestTypeOfArrayItemUnions(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			// What jsonschema-go renders for a Go []*T field.
			"nullable array of nullable objects",
			`{"type":["null","array"],"items":{"type":["null","object"],"properties":{"a":{"type":"string"}},"required":["a"]}}`,
			"({\n  a: string;\n} | null)[] | null",
		},
		{
			"array of an object union",
			`{"type":"array","items":{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"number"}}}]}}`,
			"({\n  a?: string;\n} | {\n  b?: number;\n})[]",
		},
		{
			"array of an intersection",
			`{"type":"array","items":{"allOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"number"}}}]}}`,
			"({\n  a?: string;\n} & {\n  b?: number;\n})[]",
		},
		{
			"array of an enum",
			`{"type":"array","items":{"enum":["x","y"]}}`,
			`("x" | "y")[]`,
		},
		{
			"object item with a union property",
			`{"type":"array","items":{"type":"object","properties":{"v":{"type":["string","null"]}}}}`,
			"{\n  v?: string | null;\n}[]",
		},
		{
			"record of a union",
			`{"type":"array","items":{"type":"object","additionalProperties":{"type":["string","number"]}}}`,
			"Record<string, string | number>[]",
		},
		{
			"nested arrays of a union",
			`{"type":"array","items":{"type":"array","items":{"type":["string","null"]}}}`,
			"(string | null)[][]",
		},
	}
	for _, tc := range cases {
		if got := typeOfJSON(t, tc.src); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// anyOf, oneOf and allOf narrow the rest of the schema instead of
// replacing it.
func TestTypeOfCombinatorsKeepBase(t *testing.T) {
	ab := "{\n  a?: string;\n  b?: string;\n}"
	cases := []struct{ name, src, want string }{
		{
			"oneOf as a required-property constraint",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"oneOf":[{"required":["a"]},{"required":["b"]}]}`,
			ab,
		},
		{
			"allOf with only required",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"allOf":[{"required":["a"]}]}`,
			ab,
		},
		{
			"base and a union of objects",
			`{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"],"anyOf":[{"type":"object","properties":{"x":{"type":"number"}}},{"type":"object","properties":{"y":{"type":"number"}}}]}`,
			"{\n  kind: string;\n} & ({\n  x?: number;\n} | {\n  y?: number;\n})",
		},
		{
			"base and allOf",
			`{"type":"object","properties":{"a":{"type":"string"}},"allOf":[{"type":"object","properties":{"b":{"type":"string"}}}]}`,
			"{\n  a?: string;\n} & {\n  b?: string;\n}",
		},
		{
			"bare object base adds nothing",
			`{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"string"}}}]}`,
			"{\n  a?: string;\n} | {\n  b?: string;\n}",
		},
		{
			"no base",
			`{"description":"either","anyOf":[{"type":"string"},{"type":"integer"}]}`,
			"string | number",
		},
		{
			"unknown alternatives are dropped",
			`{"anyOf":[{"type":"string"},{"required":["a"]}]}`,
			"string",
		},
		{
			"nothing known at all",
			`{"anyOf":[{"required":["a"]},{"required":["b"]}]}`,
			"unknown",
		},
	}
	for _, tc := range cases {
		if got := typeOfJSON(t, tc.src); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	// The narrowed object is still an argument type, not unknown.
	got := Signature(Member{Name: "pick", Input: schema(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"oneOf":[{"required":["a"]},{"required":["b"]}]}`)}, "")
	if !strings.HasPrefix(got, "pick(args: {\n  a?: string;") {
		t.Errorf("signature: %s", got)
	}
}

// Refs are limited per cycle, not in total: many properties may share one
// definition.
func TestTypeOfSharedRefs(t *testing.T) {
	props := map[string]any{}
	for i := range 40 {
		props[fmt.Sprintf("p%02d", i)] = map[string]any{"$ref": "#/$defs/S"}
	}
	s := Schema{"type": "object", "properties": props, "$defs": map[string]any{"S": map[string]any{"type": "string"}}}
	got := TypeOf(s, "")
	for i := range 40 {
		if want := fmt.Sprintf("p%02d?: string;", i); !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, "unknown") {
		t.Errorf("shared ref rendered unknown:\n%s", got)
	}

	// Siblings referring to the enclosing definition are cycles.
	tree := schema(t, `{"$ref":"#/$defs/Node","$defs":{"Node":{"type":"object","properties":{"left":{"$ref":"#/$defs/Node"},"right":{"$ref":"#/$defs/Node"},"label":{"$ref":"#/$defs/Label"}}},"Label":{"type":"string"}}}`)
	want := "{\n  label?: string;\n  left?: unknown;\n  right?: unknown;\n}"
	if got := TypeOf(tree, ""); got != want {
		t.Errorf("tree:\n got %q\nwant %q", got, want)
	}
	// Mutual recursion terminates too.
	mutual := schema(t, `{"$ref":"#/$defs/A","$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},"B":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}}}}}`)
	if got := TypeOf(mutual, ""); got != "{\n  b?: {\n    a?: unknown;\n  };\n}" {
		t.Errorf("mutual: %q", got)
	}
}

// Values in doc comments cannot end the comment early and print as JSON.
func TestDocCommentValues(t *testing.T) {
	got := typeOfJSON(t, `{
		"type":"object",
		"properties":{
			"path":{"type":"string","pattern":"^.*/[^/]+$","description":"A path; ends */ here"},
			"size":{"type":"number","minimum":0.5,"maximum":1000000},
			"big":{"type":"integer","default":12345678901234567890},
			"when":{"type":"string","format":"date-time","default":"a*/b"},
			"opts":{"type":"object","default":{"k":[1,2]}},
			"pick":{"enum":[{"k":1},"x",2]}
		}
	}`)
	for _, want := range []string{
		`/** A path; ends *\/ here @pattern ^.*\/[^/]+$ */`,
		`/** @minimum 0.5 @maximum 1000000 */`,
		`/** @default 12345678901234567000 */`,
		`/** @default "a*\/b" @format date-time */`,
		`/** @default {"k":[1,2]} */`,
		`pick?: {"k":1} | "x" | 2;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Every comment closes exactly once, at the end of its line.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "/**") && (strings.Count(line, "*/") != 1 || !strings.HasSuffix(line, " */")) {
			t.Errorf("comment broken: %q", line)
		}
	}
	if strings.Contains(got, "e+") || strings.Contains(got, "map[") {
		t.Errorf("Go formatting leaked:\n%s", got)
	}
}

// Two names that become the same identifier are declared once, as the
// sandbox binds only the first.
func TestDeclarationDuplicateIdentifiers(t *testing.T) {
	d := Declaration("ns", []Member{
		{Name: "a-b", Description: "First."},
		{Name: "a_b", Description: "Second."},
		{Name: "c", Description: "Third."},
		{Name: "a.b", Description: "Fourth."},
	})
	if n := strings.Count(d, "a_b("); n != 1 {
		t.Errorf("a_b declared %d times:\n%s", n, d)
	}
	for _, want := range []string{"/** First. */\n  a_b(): Promise<unknown>;", "/** Third. */\n  c(): Promise<unknown>;"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in:\n%s", want, d)
		}
	}
	if strings.Contains(d, "Second.") || strings.Contains(d, "Fourth.") {
		t.Errorf("duplicates declared:\n%s", d)
	}
	if strings.Contains(d, "\n\n\n") || !strings.HasSuffix(d, "Promise<unknown>;\n};") {
		t.Errorf("layout:\n%s", d)
	}
}
