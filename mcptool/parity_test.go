package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type parityOut struct {
	Value  int    `json:"value"`
	Status string `json:"status,omitempty"`
}

type parityIn struct {
	Name  string `json:"name"`
	Inner struct {
		Tag string `json:"tag"`
	} `json:"inner"`
}

// A tool called from a script must behave as the same tool called by a
// client: for each case, Registry.Call and UnwrapResult(CallTool) over an
// in-memory session agree on the value and on failing, and the script-side
// result has the same text blocks as the one on the wire.
func TestScriptCallsMatchWireCalls(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "parity", Version: "0"}, nil)
	reg := New(srv, Options{Mode: ModeOn})

	outSchema, err := jsonschema.For[parityOut](nil)
	if err != nil {
		t.Fatal(err)
	}
	outSchema.Properties["status"].Default = json.RawMessage(`"ok"`)
	outSchema.Properties["value"].Maximum = new(10.0)
	AddTool(reg, &mcp.Tool{Name: "out_schema", OutputSchema: outSchema}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		V int `json:"v"`
	}) (*mcp.CallToolResult, parityOut, error) {
		return nil, parityOut{Value: in.V}, nil
	})
	AddTool(reg, &mcp.Tool{Name: "nil_ptr"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, *parityOut, error) {
		return nil, nil, nil
	})
	AddTool(reg, &mcp.Tool{Name: "both"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, parityOut, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"from": "result"}}, parityOut{Value: 1}, nil
	})
	AddTool(reg, &mcp.Tool{Name: "case", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *mcp.CallToolRequest, in parityIn) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"name": in.Name, "tag": in.Inner.Tag}, nil
	})
	AddTool(reg, &mcp.Tool{Name: "list"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, []string, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "two items"}}}, []string{"a", "b"}, nil
	})
	AddTool(reg, &mcp.Tool{Name: "nil_map"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, map[string]any, error) {
		return nil, nil, nil
	})
	AddTool(reg, &mcp.Tool{Name: "any_nil"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return nil, (*parityOut)(nil), nil
	})
	AddTool(reg, &mcp.Tool{Name: "count"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, int, error) {
		return nil, 5, nil
	})
	greet, _ := jsonschema.For[greetIn](nil)
	greet.Properties["style"].Enum = []any{"formal", "casual"}
	greet.Properties["style"].Default = json.RawMessage(`"casual"`)
	AddTool(reg, &mcp.Tool{Name: "greet", InputSchema: greet}, func(_ context.Context, req *mcp.CallToolRequest, in greetIn) (*mcp.CallToolResult, any, error) {
		// The handler sees the arguments as sent; In has the defaults.
		return nil, map[string]any{"style": in.Style, "sent": string(req.Params.Arguments)}, nil
	})
	AddTool(reg, &mcp.Tool{Name: "fails"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("deliberate")
	})
	reg.AddTool(&mcp.Tool{Name: "raw", InputSchema: greet}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
	})
	cs := connect(t, srv)

	cases := []struct {
		name, tool, args string
		want             string // expected value, "" when the call must fail
	}{
		{"output default applied", "out_schema", `{"v":3}`, `{"value":3,"status":"ok"}`},
		{"output maximum enforced", "out_schema", `{"v":11}`, ""},
		{"input validated", "out_schema", `{"v":"three"}`, ""},
		{"no arguments", "out_schema", ``, ""}, // v is required
		{"typed nil pointer is the zero value", "nil_ptr", `{}`, `{"value":0}`},
		{"output replaces structured content", "both", `{}`, `{"value":1}`},
		{"field names are case-sensitive", "case", `{"NAME":"x"}`, `{"name":"","tag":""}`},
		{"nested field names are case-sensitive", "case", `{"name":"x","inner":{"TAG":"t"}}`, `{"name":"x","tag":""}`},
		{"exact field names decode", "case", `{"name":"x","inner":{"tag":"t"}}`, `{"name":"x","tag":"t"}`},
		{"non-object output keeps content", "list", `{}`, `["a","b"]`},
		{"nil map with object schema", "nil_map", `{}`, `{}`},
		{"typed nil without schema", "any_nil", `{}`, `"null"`},
		{"primitive output", "count", ``, `5`},
		{"input default applied", "greet", `{"name":"ada"}`, `{"style":"casual","sent":"{\"name\":\"ada\"}"}`},
		{"input enum enforced", "greet", `{"name":"ada","style":"loud"}`, ""},
		{"handler error", "fails", `{}`, ""},
		{"untyped: no validation", "raw", `{"style":"loud"}`, `{"style":"loud"}`},
		{"untyped: no defaults", "raw", `{"name":"ada"}`, `{"name":"ada"}`},
	}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var args json.RawMessage
			var params any
			if tc.args != "" {
				args = json.RawMessage(tc.args)
				params = args
			}
			script, scriptErr := reg.Call(ctx, tc.tool, args)
			wireRes, wireErr := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: params})
			wire, wireErr := UnwrapResult(wireRes, wireErr)
			if (scriptErr != nil) != (wireErr != nil) {
				t.Fatalf("script %s (err %v) vs wire %s (err %v)", script, scriptErr, wire, wireErr)
			}
			if tc.want == "" {
				if scriptErr == nil {
					t.Fatalf("want an error, got %s", script)
				}
				return
			}
			if scriptErr != nil {
				t.Fatalf("script: %v", scriptErr)
			}
			if !sameJSON(script, wire) || !sameJSON(script, json.RawMessage(tc.want)) {
				t.Errorf("script %s, wire %s, want %s", script, wire, tc.want)
			}
			res, err := reg.find(tc.tool).call(ctx, args)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := texts(res), texts(wireRes); !reflect.DeepEqual(got, want) {
				t.Errorf("text blocks: script %q, wire %q", got, want)
			}
		})
	}
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func texts(res *mcp.CallToolResult) []string {
	out := []string{}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

// A result that asks for input keeps the handler's output out of the
// result, as the SDK does.
func TestInputRequestsSkipOutput(t *testing.T) {
	res, err := shapeResult(&mcp.CallToolResult{InputRequests: mcp.InputRequestMap{}}, parityOut{Value: 1}, nil, nil)
	if err != nil || res.StructuredContent != nil || res.Content != nil {
		t.Errorf("got %+v %v", res, err)
	}
}
