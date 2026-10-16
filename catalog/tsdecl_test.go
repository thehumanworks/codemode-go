package catalog

import (
	"encoding/json"
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
