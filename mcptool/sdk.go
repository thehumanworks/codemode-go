package mcptool

// This file mirrors what the MCP Go SDK (v1.8.0) does around a tool call, so
// that a tool called from a script behaves as the same tool called by a
// client: setSchema, applySchema and the result shaping of toolForErr in
// mcp/server.go and mcp/tool.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	codemode "github.com/thehumanworks/codemode-go"
)

// requestKey holds the code tool's *mcp.CallToolRequest in the context of a
// script run, so tool calls made by the script carry its Session and Extra.
type requestKey struct{}

// toolRequest builds the request a script's tool call passes to the
// handler: the tool's name and the arguments as sent, with the Session and
// Extra of the code tool's request when ctx comes from a script run.
func toolRequest(ctx context.Context, name string, args json.RawMessage) *mcp.CallToolRequest {
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name, Arguments: args}}
	if outer, ok := ctx.Value(requestKey{}).(*mcp.CallToolRequest); ok && outer != nil {
		req.Session, req.Extra = outer.Session, outer.Extra
	}
	return req
}

// recoverCall turns a panic in a tool call into an error, so a faulty
// handler rejects the script's call instead of killing the process. It must
// be deferred directly.
func recoverCall(name string, res **mcp.CallToolResult, err *error) {
	if p := recover(); p != nil {
		msg, _ := codemode.Clip(fmt.Sprint(p), 500)
		*res, *err = nil, fmt.Errorf("tool %q panicked: %s", name, msg)
	}
}

// objectArgs maps missing arguments to an empty object, as a client that
// sends no arguments gets.
func objectArgs(args json.RawMessage) json.RawMessage {
	switch strings.TrimSpace(string(args)) {
	case "", "null", "undefined":
		return json.RawMessage("{}")
	}
	return args
}

// sdkCheck runs the SDK's own registration checks on a throwaway server, so
// a tool the SDK rejects panics with the SDK's message in every mode, before
// anything is recorded or registered.
func sdkCheck(add func(*mcp.Server)) {
	add(mcp.NewServer(&mcp.Implementation{Name: "mcptool-check", Version: "0"}, nil))
}

// setSchema mirrors the SDK's setSchema: derive the schema from T when
// *sfield is nil (pointers are treated as their element type), resolve it,
// and, when T is a pointer, return the element type's zero value to use in
// place of a typed nil.
func setSchema[T any](sfield *any) (resolved *jsonschema.Resolved, zero any, err error) {
	rt := reflect.TypeFor[T]()
	if rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
		zero = reflect.Zero(rt).Interface()
	}
	var schema *jsonschema.Schema
	switch s := (*sfield).(type) {
	case nil:
		if schema, err = jsonschema.ForType(rt, &jsonschema.ForOptions{}); err != nil {
			return nil, zero, err
		}
		*sfield = schema
	case *jsonschema.Schema:
		schema = s
	default:
		b, err := json.Marshal(s)
		if err == nil {
			err = json.Unmarshal(b, &schema)
		}
		if err != nil {
			return nil, zero, err
		}
	}
	if schema == nil {
		return nil, zero, fmt.Errorf("schema is nil for type %v", rt)
	}
	resolved, err = schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	return resolved, zero, err
}

// applySchema mirrors the SDK's applySchema: validate data against the
// schema after applying defaults, and return the value with the defaults.
// Input must be an object; output may be any value, and a null output with
// an object schema becomes {} with its defaults.
func applySchema(data json.RawMessage, resolved *jsonschema.Resolved, forOutput bool) (json.RawMessage, error) {
	if resolved == nil {
		return data, nil
	}
	var unmarshaled any
	if !forOutput {
		v := make(map[string]any)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &v); err != nil {
				return nil, fmt.Errorf("unmarshaling arguments: %w", err)
			}
		}
		unmarshaled = v
	} else if len(data) > 0 {
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			return nil, fmt.Errorf("unmarshaling output: %w", err)
		}
	}
	appliedDefaults := false
	if _, ok := unmarshaled.(map[string]any); ok {
		if err := resolved.ApplyDefaults(&unmarshaled); err != nil {
			return nil, fmt.Errorf("applying schema defaults:\n%w", err)
		}
		appliedDefaults = true
	} else if forOutput && unmarshaled == nil && resolved.Schema().Type == "object" {
		unmarshaled = make(map[string]any)
		if err := resolved.ApplyDefaults(&unmarshaled); err != nil {
			return nil, fmt.Errorf("applying schema defaults:\n%w", err)
		}
		appliedDefaults = true
	}
	if err := resolved.Validate(&unmarshaled); err != nil {
		return nil, err
	}
	if !appliedDefaults {
		return data, nil
	}
	out, err := json.Marshal(unmarshaled)
	if err != nil {
		return nil, fmt.Errorf("marshalling with defaults: %v", err)
	}
	return out, nil
}

// isObjectJSON reports whether data is a JSON object.
func isObjectJSON(data json.RawMessage) bool {
	t := bytes.TrimLeft(data, " \t\n\r")
	return len(t) > 0 && t[0] == '{'
}

// shapeResult mirrors how the SDK turns a typed handler's (result, output)
// into the tool result: a nil result becomes empty; unless the result asks
// for input, the output (a typed nil pointer replaced by elemZero) is
// validated against the output schema with defaults applied and becomes
// StructuredContent, replacing any the handler set; Content gets the JSON
// as a text block when empty, and the JSON is appended when the output is
// not an object.
func shapeResult[Out any](res *mcp.CallToolResult, out Out, elemZero any, resolved *jsonschema.Resolved) (*mcp.CallToolResult, error) {
	if res == nil {
		res = &mcp.CallToolResult{}
	}
	var outval any = out
	if res.InputRequests != nil {
		outval = nil
	} else if elemZero != nil {
		var z Out
		if any(out) == any(z) { // elemZero is only set when Out is a pointer
			outval = elemZero
		}
	}
	if outval == nil {
		return res, nil
	}
	b, err := json.Marshal(outval)
	if err != nil {
		return nil, fmt.Errorf("marshaling output: %w", err)
	}
	outJSON, err := applySchema(b, resolved, true)
	if err != nil {
		return nil, fmt.Errorf("validating tool output: %w", err)
	}
	res.StructuredContent = outJSON
	if res.Content == nil {
		res.Content = []mcp.Content{&mcp.TextContent{Text: string(outJSON)}}
	} else if !isObjectJSON(outJSON) {
		res.Content = append(res.Content, &mcp.TextContent{Text: string(outJSON)})
	}
	return res, nil
}

// decodeInput unmarshals validated arguments into In the way the SDK does.
// The SDK's decoder matches object keys to struct fields case-sensitively,
// while encoding/json also accepts keys that differ in case; when In
// contains structs, keys that are not exactly a field's JSON name are
// dropped first.
func decodeInput[In any](data json.RawMessage, in *In) error {
	if t := reflect.TypeFor[In](); hasStruct(t) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber() // re-encode numbers exactly
		var v any
		if err := dec.Decode(&v); err != nil {
			return err
		}
		b, err := json.Marshal(exactKeys(v, t))
		if err != nil {
			return err
		}
		data = b
	}
	return json.Unmarshal(data, in)
}

func hasStruct(t reflect.Type) bool {
	for range 16 {
		switch t.Kind() {
		case reflect.Struct:
			return true
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
		default:
			return false
		}
	}
	return false
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// exactKeys removes, in place, the keys of objects decoded for struct types
// that do not exactly match one of the struct's JSON field names.
func exactKeys(v any, t reflect.Type) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return v
	}
	switch t.Kind() {
	case reflect.Struct:
		if obj, ok := v.(map[string]any); ok {
			fields := jsonFields(t)
			for k, x := range obj {
				if ft, ok := fields[k]; ok {
					obj[k] = exactKeys(x, ft)
				} else {
					delete(obj, k)
				}
			}
		}
	case reflect.Map:
		if obj, ok := v.(map[string]any); ok {
			for k, x := range obj {
				obj[k] = exactKeys(x, t.Elem())
			}
		}
	case reflect.Slice, reflect.Array:
		if arr, ok := v.([]any); ok {
			for i, x := range arr {
				arr[i] = exactKeys(x, t.Elem())
			}
		}
	}
	return v
}

var fieldCache sync.Map // reflect.Type → map[string]reflect.Type

// jsonFields maps the JSON names of a struct's fields, including those
// promoted from embedded structs (the shallowest wins), to their types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	if f, ok := fieldCache.Load(t); ok {
		return f.(map[string]reflect.Type)
	}
	fields := map[string]reflect.Type{}
	depth := map[string]int{}
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, d int)
	walk = func(t reflect.Type, d int) {
		if seen[t] {
			return
		}
		seen[t] = true
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				et := f.Type
				if et.Kind() == reflect.Pointer {
					et = et.Elem()
				}
				if et.Kind() == reflect.Struct {
					walk(et, d+1)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if old, ok := depth[name]; !ok || d < old {
				fields[name], depth[name] = f.Type, d
			}
		}
	}
	walk(t, 0)
	fieldCache.Store(t, fields)
	return fields
}
