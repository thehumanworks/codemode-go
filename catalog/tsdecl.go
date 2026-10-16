package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Schema is a JSON Schema as decoded from JSON.
type Schema = map[string]any

const (
	maxTypeDepth = 8
	indentUnit   = "  "
)

var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// Identifier turns a tool name into a JavaScript identifier: characters that
// cannot appear in one become underscores.
func Identifier(name string) string {
	var b strings.Builder
	for i, r := range name {
		ok := r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		return "_"
	}
	return s
}

type tsCtx struct {
	root  Schema
	depth int
	refs  int
}

// TypeOf renders a JSON Schema as a TypeScript type expression. Objects
// render multi-line when they have properties; indent is the current
// indentation of the enclosing line.
func TypeOf(schema any, indent string) string {
	root, _ := schema.(map[string]any)
	return (&tsCtx{root: root}).typeOf(schema, indent)
}

func (c *tsCtx) typeOf(schema any, indent string) string {
	switch s := schema.(type) {
	case bool:
		if s {
			return "unknown"
		}
		return "never"
	case map[string]any:
		if c.depth >= maxTypeDepth {
			return "unknown"
		}
		c.depth++
		defer func() { c.depth-- }()
		return c.object(s, indent)
	default:
		return "unknown"
	}
}

func (c *tsCtx) object(s Schema, indent string) string {
	if ref, ok := s["$ref"].(string); ok {
		c.refs++
		if c.refs > 32 {
			return "unknown"
		}
		if target := resolveRef(ref, c.root); target != nil {
			return c.typeOf(target, indent)
		}
		return "unknown"
	}
	if v, ok := s["const"]; ok {
		return literal(v)
	}
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 {
		parts := make([]string, 0, len(enum))
		for _, v := range enum {
			parts = append(parts, literal(v))
		}
		return strings.Join(parts, " | ")
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := s[key].([]any); ok && len(alts) > 0 {
			return c.union(alts, indent, " | ")
		}
	}
	if alts, ok := s["allOf"].([]any); ok && len(alts) > 0 {
		return c.union(alts, indent, " & ")
	}

	types := typeList(s["type"])
	nullable := false
	var kinds []string
	for _, t := range types {
		if t == "null" {
			nullable = true
		} else {
			kinds = append(kinds, t)
		}
	}
	if len(kinds) == 0 {
		switch {
		case s["properties"] != nil || s["additionalProperties"] != nil:
			kinds = []string{"object"}
		case s["items"] != nil:
			kinds = []string{"array"}
		case nullable:
			return "null"
		default:
			return "unknown"
		}
	}
	parts := make([]string, 0, len(kinds)+1)
	for _, k := range kinds {
		parts = append(parts, c.kind(k, s, indent))
	}
	if nullable {
		parts = append(parts, "null")
	}
	out := strings.Join(parts, " | ")
	return out
}

func (c *tsCtx) union(alts []any, indent, sep string) string {
	parts := make([]string, 0, len(alts))
	seen := map[string]bool{}
	for _, a := range alts {
		t := c.typeOf(a, indent)
		if !seen[t] {
			seen[t] = true
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, sep)
}

func (c *tsCtx) kind(k string, s Schema, indent string) string {
	switch k {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		item := "unknown"
		if it, ok := s["items"]; ok && it != nil {
			if list, isList := it.([]any); isList {
				// Tuple form.
				parts := make([]string, 0, len(list))
				for _, e := range list {
					parts = append(parts, c.typeOf(e, indent))
				}
				return "[" + strings.Join(parts, ", ") + "]"
			}
			item = c.typeOf(it, indent)
		}
		if strings.ContainsAny(item, " |&") && !strings.HasPrefix(item, "{") {
			return "(" + item + ")[]"
		}
		return item + "[]"
	case "object":
		props, _ := s["properties"].(map[string]any)
		if len(props) == 0 {
			if ap, ok := s["additionalProperties"]; ok {
				if b, isBool := ap.(bool); isBool && !b {
					return "Record<string, never>"
				}
				if ap != nil {
					return "Record<string, " + c.typeOf(ap, indent) + ">"
				}
			}
			return "Record<string, unknown>"
		}
		required := map[string]bool{}
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if name, isStr := r.(string); isStr {
					required[name] = true
				}
			}
		}
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		inner := indent + indentUnit
		var b strings.Builder
		b.WriteString("{\n")
		for _, name := range names {
			p := props[name]
			if ps, ok := p.(map[string]any); ok {
				if doc := docLine(ps); doc != "" {
					b.WriteString(inner + "/** " + doc + " */\n")
				}
			}
			opt := ""
			if !required[name] {
				opt = "?"
			}
			b.WriteString(inner + quoteProp(name) + opt + ": " + c.typeOf(p, inner) + ";\n")
		}
		b.WriteString(indent + "}")
		return b.String()
	}
	return "unknown"
}

// docLine builds the comment for a property: its description plus the
// constraints worth knowing (default, format, bounds).
func docLine(s Schema) string {
	var parts []string
	if d, ok := s["description"].(string); ok && strings.TrimSpace(d) != "" {
		parts = append(parts, strings.ReplaceAll(strings.TrimSpace(d), "*/", "*\\/"))
	}
	if v, ok := s["default"]; ok && v != nil {
		parts = append(parts, "@default "+literal(v))
	}
	for _, key := range []string{"format", "pattern", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"} {
		if v, ok := s[key]; ok && v != nil {
			parts = append(parts, fmt.Sprintf("@%s %v", key, v))
		}
	}
	line := strings.Join(parts, " ")
	line = strings.ReplaceAll(line, "\n", " ")
	if len(line) > 300 {
		line = line[:297] + "..."
	}
	return line
}

func typeList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func literal(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func quoteProp(name string) string {
	if identRe.MatchString(name) {
		return name
	}
	return fmt.Sprintf("%q", name)
}

// resolveRef follows a local JSON Pointer such as #/$defs/Foo.
func resolveRef(ref string, root Schema) any {
	if root == nil || !strings.HasPrefix(ref, "#") {
		return nil
	}
	if ref == "#" {
		return root
	}
	var cur any = root
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[seg]
		if !ok {
			return nil
		}
	}
	return cur
}

// Member is one method of a namespace declaration.
type Member struct {
	Name        string
	Description string
	Input       any // JSON Schema of the single argument; nil for no argument
	Output      any // JSON Schema of the resolved value; nil for unknown
}

// Signature renders `name(args: In): Promise<Out>;` on one logical line.
func Signature(m Member, indent string) string {
	in := "unknown"
	if m.Input != nil {
		in = TypeOf(m.Input, indent)
	}
	out := "unknown"
	if m.Output != nil {
		out = TypeOf(m.Output, indent)
	}
	args := "args: " + in
	if m.Input == nil {
		args = ""
	}
	return fmt.Sprintf("%s(%s): Promise<%s>;", Identifier(m.Name), args, out)
}

// Declaration renders a `declare const ns: { ... };` block with a JSDoc
// comment per member.
func Declaration(ns string, members []Member) string {
	var b strings.Builder
	fmt.Fprintf(&b, "declare const %s: {\n", Identifier(ns))
	for i, m := range members {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(DocComment(m.Description, indentUnit))
		b.WriteString(indentUnit + Signature(m, indentUnit) + "\n")
	}
	b.WriteString("};")
	return b.String()
}

// DocComment renders a JSDoc block, or nothing for an empty description.
func DocComment(text, indent string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "*/", "*\\/"))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) == 1 {
		return indent + "/** " + lines[0] + " */\n"
	}
	var b strings.Builder
	b.WriteString(indent + "/**\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			b.WriteString(indent + " *\n")
		} else {
			b.WriteString(indent + " * " + l + "\n")
		}
	}
	b.WriteString(indent + " */\n")
	return b.String()
}
