package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Schema is a JSON Schema as decoded from JSON.
type Schema = map[string]any

const (
	maxTypeDepth = 8
	// maxRefExpansions bounds the total $ref expansions in one TypeOf call,
	// so a schema whose definitions fan out cannot blow up the output.
	// Cycles are cut separately, by the per-ref stack in tsCtx.
	maxRefExpansions = 1000
	maxDocLine       = 300
	indentUnit       = "  "
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

// tsType is a rendered TypeScript type plus its top-level operator, so the
// caller can decide whether it needs parentheses.
type tsType struct {
	s  string
	op byte // 0, '|' for a union or '&' for an intersection at the top level
}

var unknownType = tsType{s: "unknown"}

// arrayItem renders t as the element of an array type: any top-level
// operator needs parentheses, since `A | B[]` means `A | (B[])`.
func (t tsType) arrayItem() string {
	if t.op != 0 {
		return "(" + t.s + ")"
	}
	return t.s
}

// intersectionOperand renders t as an operand of `&`, which binds tighter
// than `|`.
func (t tsType) intersectionOperand() string {
	if t.op == '|' {
		return "(" + t.s + ")"
	}
	return t.s
}

// joinTypes joins distinct types with op (" | " or " & "); a single type
// keeps its own operator.
func joinTypes(parts []tsType, op byte) tsType {
	var uniq []tsType
	seen := map[string]bool{}
	for _, p := range parts {
		if !seen[p.s] {
			seen[p.s] = true
			uniq = append(uniq, p)
		}
	}
	switch len(uniq) {
	case 0:
		return unknownType
	case 1:
		return uniq[0]
	}
	texts := make([]string, len(uniq))
	for i, p := range uniq {
		if op == '&' {
			texts[i] = p.intersectionOperand()
		} else {
			texts[i] = p.s
		}
	}
	return tsType{s: strings.Join(texts, " "+string(op)+" "), op: op}
}

type tsCtx struct {
	root     Schema
	depth    int
	active   map[string]int // $refs being expanded, to cut cycles
	expanded int            // $ref expansions so far
}

// TypeOf renders a JSON Schema as a TypeScript type expression. Objects
// render multi-line when they have properties; indent is the current
// indentation of the enclosing line.
func TypeOf(schema any, indent string) string {
	root, _ := schema.(map[string]any)
	return (&tsCtx{root: root, active: map[string]int{}}).typeOf(schema, indent).s
}

func (c *tsCtx) typeOf(schema any, indent string) tsType {
	switch s := schema.(type) {
	case bool:
		if s {
			return unknownType
		}
		return tsType{s: "never"}
	case map[string]any:
		if c.depth >= maxTypeDepth {
			return unknownType
		}
		c.depth++
		defer func() { c.depth-- }()
		return c.object(s, indent)
	default:
		return unknownType
	}
}

// ref expands a $ref. A ref already being expanded further up is a cycle
// and renders as unknown; a ref used many times side by side is expanded
// each time.
func (c *tsCtx) ref(ref, indent string) tsType {
	if c.active[ref] > 0 || c.expanded >= maxRefExpansions {
		return unknownType
	}
	target := resolveRef(ref, c.root)
	if target == nil {
		return unknownType
	}
	c.active[ref]++
	c.expanded++
	defer func() { c.active[ref]-- }()
	return c.typeOf(target, indent)
}

var combinators = []string{"allOf", "anyOf", "oneOf"}

func hasCombinator(s Schema) bool {
	for _, key := range combinators {
		if alts, ok := s[key].([]any); ok && len(alts) > 0 {
			return true
		}
	}
	return false
}

func (c *tsCtx) object(s Schema, indent string) tsType {
	if ref, ok := s["$ref"].(string); ok {
		return c.ref(ref, indent)
	}
	if v, ok := s["const"]; ok {
		return tsType{s: literal(v)}
	}
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 {
		parts := make([]tsType, 0, len(enum))
		for _, v := range enum {
			parts = append(parts, tsType{s: literal(v)})
		}
		return joinTypes(parts, '|')
	}
	if hasCombinator(s) {
		return c.combined(s, indent)
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
			return tsType{s: "null"}
		default:
			return unknownType
		}
	}
	parts := make([]tsType, 0, len(kinds)+1)
	for _, k := range kinds {
		parts = append(parts, tsType{s: c.kind(k, s, indent)})
	}
	if nullable {
		parts = append(parts, tsType{s: "null"})
	}
	return joinTypes(parts, '|')
}

// combined renders a schema with allOf, anyOf or oneOf: the rest of the
// schema is the base, and the combinators narrow it, as
// `Base & A & B & (C | D)`. Alternatives that render as unknown (pure
// constraints such as {required: [...]}) are dropped; a plain object base
// adds nothing next to a more specific alternative.
func (c *tsCtx) combined(s Schema, indent string) tsType {
	base := make(Schema, len(s))
	for k, v := range s {
		base[k] = v
	}
	for _, key := range combinators {
		delete(base, key)
	}
	var parts []tsType
	if b := c.object(base, indent); b.s != "unknown" {
		parts = append(parts, b)
	}
	for _, key := range combinators {
		alts, _ := s[key].([]any)
		var known []tsType
		for _, a := range alts {
			if t := c.typeOf(a, indent); t.s != "unknown" {
				known = append(known, t)
			}
		}
		if len(known) == 0 {
			continue
		}
		if key == "allOf" {
			parts = append(parts, known...)
		} else {
			parts = append(parts, joinTypes(known, '|'))
		}
	}
	if len(parts) > 1 && parts[0].s == "Record<string, unknown>" {
		parts = parts[1:]
	}
	return joinTypes(parts, '&')
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
		item := unknownType
		if it, ok := s["items"]; ok && it != nil {
			if list, isList := it.([]any); isList {
				// Tuple form.
				parts := make([]string, 0, len(list))
				for _, e := range list {
					parts = append(parts, c.typeOf(e, indent).s)
				}
				return "[" + strings.Join(parts, ", ") + "]"
			}
			item = c.typeOf(it, indent)
		}
		return item.arrayItem() + "[]"
	case "object":
		props, _ := s["properties"].(map[string]any)
		if len(props) == 0 {
			if ap, ok := s["additionalProperties"]; ok {
				if b, isBool := ap.(bool); isBool && !b {
					return "Record<string, never>"
				}
				if ap != nil {
					return "Record<string, " + c.typeOf(ap, indent).s + ">"
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
			b.WriteString(inner + quoteProp(name) + opt + ": " + c.typeOf(p, inner).s + ";\n")
		}
		b.WriteString(indent + "}")
		return b.String()
	}
	return "unknown"
}

// docLine builds the comment for a property: its description plus the
// constraints worth knowing (default, format, bounds). String constraints
// print as they are, other values as JSON; nothing in the line can end the
// comment it goes into.
func docLine(s Schema) string {
	var parts []string
	if d, ok := s["description"].(string); ok && strings.TrimSpace(d) != "" {
		parts = append(parts, strings.TrimSpace(d))
	}
	if v, ok := s["default"]; ok && v != nil {
		parts = append(parts, "@default "+literal(v))
	}
	for _, key := range []string{"format", "pattern", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"} {
		v, ok := s[key]
		if !ok || v == nil {
			continue
		}
		text, isStr := v.(string)
		if !isStr {
			text = literal(v)
		}
		parts = append(parts, "@"+key+" "+text)
	}
	line := strings.Join(parts, " ")
	line = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(line)
	if utf8.RuneCountInString(line) > maxDocLine {
		line = string([]rune(line)[:maxDocLine-3]) + "..."
	}
	return escapeComment(line)
}

// escapeComment keeps text from closing the block comment it is put in.
func escapeComment(text string) string {
	return strings.ReplaceAll(text, "*/", "*\\/")
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

// literal renders a JSON value as JSON, which is also a valid TypeScript
// literal type: strings quoted, numbers without exponent noise below 1e21,
// objects and arrays as object and tuple types.
func literal(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "unknown"
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func quoteProp(name string) string {
	if identRe.MatchString(name) {
		return name
	}
	return literal(name)
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
// comment per member. Members whose names map to an identifier already
// declared are skipped, as the sandbox keeps the first of them too.
func Declaration(ns string, members []Member) string {
	var b strings.Builder
	fmt.Fprintf(&b, "declare const %s: {\n", Identifier(ns))
	seen := map[string]bool{}
	for _, m := range members {
		id := Identifier(m.Name)
		if seen[id] {
			continue
		}
		if len(seen) > 0 {
			b.WriteString("\n")
		}
		seen[id] = true
		b.WriteString(DocComment(m.Description, indentUnit))
		b.WriteString(indentUnit + Signature(m, indentUnit) + "\n")
	}
	b.WriteString("};")
	return b.String()
}

// DocComment renders a JSDoc block, or nothing for an empty description.
func DocComment(text, indent string) string {
	text = strings.TrimSpace(escapeComment(text))
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
