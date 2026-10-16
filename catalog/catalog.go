package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	codemode "github.com/thehumanworks/codemode-go"
)

const (
	defaultNamespace            = "tools"
	defaultNamespaceDescription = "Tools callable from scripts; every method takes one object argument and resolves to the tool's result."
	defaultSearchLimit          = 8
	maxSearchLimit              = 25
	maxSuggestions              = 5
)

const (
	searchUsage = `search(query, {limit?}) needs a query string, e.g. await search("split a pane")`
	searchEmpty = "search: query must be a non-empty string"
)

// Entry is one discoverable thing: a namespace member, an API method, anything.
type Entry struct {
	// Path names the entry; dots separate groups, e.g. "rex.capture" or
	// "api.session.view". Entries at "<Namespace>.<name>" are namespace members.
	Path string
	// Kind is free-form, e.g. "tool" or "api_method". Members default to "tool".
	Kind        string
	Description string
	// Input is the JSON Schema of the single argument; nil means none. Any
	// value that marshals to JSON Schema works, as does raw JSON given as
	// json.RawMessage, []byte or string (unparseable JSON renders unknown).
	Input any
	// Output is the JSON Schema of the result; nil means unknown. Accepted
	// forms are those of Input.
	Output any
	// Extra is merged into describe() output for this entry, e.g. "call" or
	// examples. It cannot replace path, kind, description or types. New
	// copies the map, and nested maps and slices of JSON-like values.
	Extra map[string]any
	// Keywords is extra searchable text.
	Keywords string
}

// Options configures a Catalog.
type Options struct {
	// Namespace is the sandbox namespace; entries at "<ns>.<name>" are its
	// members. Default "tools".
	Namespace string
	// NamespaceDescription is returned by describe(ns); has a default.
	NamespaceDescription string
	// DefaultLimit is the number of search results when the script gives no
	// limit; default 8.
	DefaultLimit int
	// MaxLimit caps the number of search results; default 25.
	MaxLimit int
	// Warning, when non-empty, is added as "warning" to search and describe
	// results, e.g. to say the catalogue is partial.
	Warning string
}

func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = defaultNamespace
	}
	if o.NamespaceDescription == "" {
		o.NamespaceDescription = defaultNamespaceDescription
	}
	if o.MaxLimit <= 0 {
		o.MaxLimit = maxSearchLimit
	}
	if o.DefaultLimit <= 0 {
		o.DefaultLimit = defaultSearchLimit
	}
	if o.DefaultLimit > o.MaxLimit {
		o.DefaultLimit = o.MaxLimit
	}
	return o
}

// group is a top-level path segment shared by non-member entries, such as "api".
type group struct {
	name    string
	label   string // what describe(name) lists, e.g. "api method"
	example string // the first entry path under it
}

// toolsAlias is the target that names the namespace whatever it is called,
// unless an entry path claims it (see Catalog.Describe).
const toolsAlias = "tools"

// Catalog is an immutable, searchable set of entries. It is safe for
// concurrent use. The decoded schemas that Entries, Lookup and Members return
// are shared with the catalogue by reference and must not be mutated.
type Catalog struct {
	opts    Options
	entries []Entry
	byPath  map[string]int
	members []Member
	groups  []group
	index   *Index
	// aliasActive is true when "tools" names the namespace: the namespace is
	// "tools", or no entry path starts with a "tools" segment.
	aliasActive bool
	// nsTerms are the query tokens that only name the namespace, which every
	// member matches; Search and did-you-mean ignore them.
	nsTerms map[string]bool
}

// New builds a catalogue. Schemas are normalised once through a JSON round
// trip and Extra maps are copied, so later changes to the given entries do
// not reach the catalogue. Entries with an empty Path are skipped; for a
// repeated Path the first entry wins.
func New(entries []Entry, opts Options) *Catalog {
	opts = opts.withDefaults()
	c := &Catalog{opts: opts, byPath: map[string]int{}, nsTerms: map[string]bool{}}
	toolsSegment := false
	kinds := map[string]string{}
	mixed := map[string]bool{}
	examples := map[string]string{}
	docs := make([]Document, 0, len(entries))
	for _, e := range entries {
		e.Path = strings.TrimSpace(e.Path)
		if e.Path == "" {
			continue
		}
		if _, dup := c.byPath[e.Path]; dup {
			continue
		}
		e.Input = normalize(e.Input)
		e.Output = normalize(e.Output)
		e.Extra = cloneExtra(e.Extra)
		if first, _, _ := strings.Cut(e.Path, "."); first == toolsAlias {
			toolsSegment = true
		}
		if name, ok := c.memberName(e.Path); ok {
			if e.Kind == "" {
				e.Kind = "tool"
			}
			c.members = append(c.members, Member{Name: name, Description: e.Description, Input: e.Input, Output: e.Output})
		} else if g, _, ok := strings.Cut(e.Path, "."); ok && g != opts.Namespace {
			if k, seen := kinds[g]; !seen {
				kinds[g] = e.Kind
				examples[g] = e.Path
			} else if k != e.Kind {
				mixed[g] = true
			}
		}
		c.byPath[e.Path] = len(c.entries)
		c.entries = append(c.entries, e)
		docs = append(docs, Document{Path: e.Path, Text: searchText(e)})
	}
	for name, kind := range kinds {
		label := name + ".* entry"
		if !mixed[name] && kind != "" {
			label = strings.ReplaceAll(kind, "_", " ")
		}
		c.groups = append(c.groups, group{name: name, label: label, example: examples[name]})
	}
	sort.Slice(c.groups, func(i, j int) bool { return c.groups[i].name < c.groups[j].name })
	c.index = NewIndex(docs)
	c.aliasActive = opts.Namespace == toolsAlias || !toolsSegment
	nsWords := opts.Namespace
	if c.aliasActive {
		nsWords += " " + toolsAlias
	}
	for _, t := range Tokenize(nsWords) {
		c.nsTerms[t] = true
	}
	return c
}

// Entries returns a copy of the entries in order, with schemas normalised.
// Each Extra map is a copy; Input and Output are shared with the catalogue
// and must not be mutated.
func (c *Catalog) Entries() []Entry {
	out := make([]Entry, len(c.entries))
	for i, e := range c.entries {
		e.Extra = cloneExtra(e.Extra)
		out[i] = e
	}
	return out
}

// Lookup returns the entry with exactly this path. Its Extra map is a copy;
// Input and Output are shared with the catalogue and must not be mutated.
func (c *Catalog) Lookup(path string) (Entry, bool) {
	i, ok := c.byPath[path]
	if !ok {
		return Entry{}, false
	}
	e := c.entries[i]
	e.Extra = cloneExtra(e.Extra)
	return e, true
}

// Members returns the namespace members, as declaration members, in entry order.
func (c *Catalog) Members() []Member {
	return append([]Member(nil), c.members...)
}

// memberName reports whether path is "<ns>.<name>" with no further dot.
func (c *Catalog) memberName(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, c.opts.Namespace+".")
	if !ok || name == "" || strings.Contains(name, ".") {
		return "", false
	}
	return name, true
}

// Hit is one search result.
type Hit struct {
	Path        string  `json:"path"`
	Kind        string  `json:"kind"`
	Description string  `json:"description"` // first sentence
	Score       float64 `json:"score"`       // rounded to 2 decimals
}

// SearchResult is what search() resolves to.
type SearchResult struct {
	Results   []Hit  `json:"results"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Warning   string `json:"warning,omitempty"`
	Hint      string `json:"hint,omitempty"` // on zero results: suggests describe(ns) and the groups
}

// Search ranks entries against query with BM25. limit <= 0 means
// DefaultLimit; it is capped at MaxLimit. Total counts every match. Words
// that only name the namespace (its name, and "tools" when that is an alias
// for it) are ignored, since every member would match them.
func (c *Catalog) Search(query string, limit int) SearchResult {
	if limit <= 0 {
		limit = c.opts.DefaultLimit
	}
	limit = min(limit, c.opts.MaxLimit)
	matches := c.index.search(c.queryTerms(query), len(c.entries))
	shown := matches[:min(limit, len(matches))]
	res := SearchResult{
		Results:   make([]Hit, 0, len(shown)),
		Total:     len(matches),
		Truncated: len(matches) > limit,
		Warning:   c.opts.Warning,
	}
	for _, m := range shown {
		e := c.entries[c.byPath[m.Path]]
		res.Results = append(res.Results, Hit{Path: e.Path, Kind: e.Kind, Description: firstSentence(e.Description), Score: round2(m.Score)})
	}
	if res.Total == 0 {
		res.Hint = c.noMatchHint()
	}
	return res
}

// queryTerms tokenizes a query without the words that name the namespace.
func (c *Catalog) queryTerms(query string) []string {
	var terms []string
	for _, t := range Tokenize(query) {
		if !c.nsTerms[t] {
			terms = append(terms, t)
		}
	}
	return terms
}

func (c *Catalog) noMatchHint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "No match. Try other words, or describe(%q) to list every tool", c.opts.Namespace)
	for _, g := range c.groups {
		fmt.Fprintf(&b, ", describe(%q) for every %s", g.name, g.label)
	}
	b.WriteString(".")
	return b.String()
}

// Describe resolves a target, in this order:
//
//	"", ns, "tools"         the namespace: {path, kind: "namespace", description, types}
//	an entry's exact path   {path, kind, description, types} plus Extra; members also get call
//	a group prefix ("api")  {path, kind: "namespace", members: ["api.x: first sentence", ...], hint}
//	a bare member name      as the member's full path
//
// "tools" names the namespace only when the namespace is "tools" or no entry
// path starts with a "tools" segment; otherwise it is resolved like any
// other target, usually as the group "tools". Anything else is an error
// that suggests close paths. The result is a fresh map each call.
func (c *Catalog) Describe(target string) (map[string]any, error) {
	target = strings.TrimSuffix(strings.TrimSpace(target), ".")
	ns := c.opts.Namespace
	if target == "" || target == ns || (target == toolsAlias && c.aliasActive) {
		return c.withWarning(map[string]any{
			"path":        ns,
			"kind":        "namespace",
			"description": c.opts.NamespaceDescription,
			"types":       c.Declaration(),
		}), nil
	}
	if i, ok := c.byPath[target]; ok {
		return c.describeEntry(c.entries[i]), nil
	}
	if lines := c.groupLines(target); len(lines) > 0 {
		return c.withWarning(map[string]any{
			"path":    target,
			"kind":    "namespace",
			"members": lines,
			"hint":    fmt.Sprintf("describe(%q) gives the argument and result types and how to call it.", target+".<name>"),
		}), nil
	}
	if !strings.Contains(target, ".") {
		if i, ok := c.byPath[ns+"."+target]; ok {
			return c.describeEntry(c.entries[i]), nil
		}
	}
	return nil, c.notFound(target)
}

func (c *Catalog) describeEntry(e Entry) map[string]any {
	out := map[string]any{"path": e.Path, "kind": e.Kind, "description": e.Description}
	if name, ok := c.memberName(e.Path); ok {
		m := Member{Name: name, Description: e.Description, Input: e.Input, Output: e.Output}
		out["types"] = Declaration(c.opts.Namespace, []Member{m})
		args := "{...}"
		if e.Input == nil {
			args = ""
		}
		out["call"] = fmt.Sprintf("await %s.%s(%s)", Identifier(c.opts.Namespace), Identifier(name), args)
	} else {
		var types strings.Builder
		if e.Input != nil {
			types.WriteString("type Args = " + TypeOf(e.Input, "") + ";\n")
		}
		if e.Output != nil {
			types.WriteString("type Result = " + TypeOf(e.Output, "") + ";")
		}
		out["types"] = strings.TrimSpace(types.String())
	}
	for k, v := range e.Extra {
		if _, taken := out[k]; !taken || k == "call" {
			out[k] = cloneValue(v)
		}
	}
	return c.withWarning(out)
}

// groupLines lists "path: first sentence" for every entry under prefix.
func (c *Catalog) groupLines(prefix string) []string {
	var lines []string
	for _, e := range c.entries {
		if !strings.HasPrefix(e.Path, prefix+".") {
			continue
		}
		line := e.Path
		if d := firstSentence(e.Description); d != "" {
			line += ": " + d
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

func (c *Catalog) notFound(target string) error {
	msg := fmt.Sprintf("describe: %q not found.", target)
	sugg := c.suggest(target)
	if len(sugg) > 0 {
		msg += " Did you mean: " + strings.Join(sugg, ", ") + "?"
	} else {
		msg += ` Use search("...") or ` + strings.Join(c.namespaceCalls(), ", ") + "."
	}
	if c.opts.Warning != "" {
		msg += " (" + c.opts.Warning + ")"
	}
	return errors.New(msg)
}

// suggest lists up to maxSuggestions paths close to an unknown target:
// first entries whose last path segment starts with, or is a small edit
// away from, the target's last segment (closest first, entries under the
// same parent before others); then BM25 matches for the target's words,
// without the words that only name the namespace.
func (c *Catalog) suggest(target string) []string {
	parent, seg := strings.ToLower(c.opts.Namespace), strings.ToLower(target)
	if i := strings.LastIndex(seg, "."); i >= 0 {
		parent, seg = seg[:i], seg[i+1:]
	}
	type near struct {
		path      string
		dist      int
		elsewhere bool
		order     int
	}
	var nearby []near
	if n := utf8.RuneCountInString(seg); n > 0 {
		maxDist := max(1, n/3)
		for i, e := range c.entries {
			ep, es := "", strings.ToLower(e.Path)
			if j := strings.LastIndex(es, "."); j >= 0 {
				ep, es = es[:j], es[j+1:]
			}
			d := editDistance(seg, es)
			if d <= maxDist || (n >= 2 && strings.HasPrefix(es, seg)) {
				nearby = append(nearby, near{path: e.Path, dist: d, elsewhere: ep != parent, order: i})
			}
		}
	}
	sort.Slice(nearby, func(i, j int) bool {
		a, b := nearby[i], nearby[j]
		if a.dist != b.dist {
			return a.dist < b.dist
		}
		if a.elsewhere != b.elsewhere {
			return !a.elsewhere
		}
		return a.order < b.order
	})
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		if len(out) < maxSuggestions && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	for _, n := range nearby {
		add(n.path)
	}
	for _, m := range c.index.search(c.queryTerms(target), maxSuggestions) {
		add(m.Path)
	}
	return out
}

// editDistance is the optimal string alignment distance between a and b,
// counted in runes: insertions, deletions, substitutions and transpositions
// of adjacent runes each cost one.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}

// namespaceCalls is describe(ns) followed by describe(group) for every group.
func (c *Catalog) namespaceCalls() []string {
	calls := []string{fmt.Sprintf("describe(%q)", c.opts.Namespace)}
	for _, g := range c.groups {
		calls = append(calls, fmt.Sprintf("describe(%q)", g.name))
	}
	return calls
}

func (c *Catalog) withWarning(out map[string]any) map[string]any {
	if c.opts.Warning != "" {
		out["warning"] = c.opts.Warning
	}
	return out
}

// Declaration renders the namespace as `declare const <ns>: { members };`.
func (c *Catalog) Declaration() string {
	return Declaration(c.opts.Namespace, c.members)
}

// Hint is a sentence for codemode.Options.Hint, appended to errors about
// namespace members that do not exist.
func (c *Catalog) Hint() string {
	return fmt.Sprintf(`Use await search("<topic>") to find tools and await describe("%s.<name>") for their arguments.`, c.opts.Namespace)
}

// Globals returns the "search" and "describe" host functions for
// codemode.Run. Both take positional JSON arguments: search(query, {limit?})
// and describe(target).
func (c *Catalog) Globals() map[string]codemode.HostFunc {
	return map[string]codemode.HostFunc{
		"search":   c.searchGlobal,
		"describe": c.describeGlobal,
	}
}

func (c *Catalog) searchGlobal(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var pos []json.RawMessage
	if err := json.Unmarshal(args, &pos); err != nil || len(pos) == 0 {
		return nil, errors.New(searchUsage)
	}
	var query string
	if err := json.Unmarshal(pos[0], &query); err != nil || strings.TrimSpace(query) == "" {
		return nil, errors.New(searchEmpty)
	}
	limit := 0
	if len(pos) > 1 {
		var opts struct {
			Limit float64 `json:"limit"`
		}
		var n float64
		if json.Unmarshal(pos[1], &opts) == nil {
			limit = int(opts.Limit)
		} else if json.Unmarshal(pos[1], &n) == nil {
			limit = int(n)
		}
	}
	return json.Marshal(c.Search(query, limit))
}

func (c *Catalog) describeGlobal(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var pos []json.RawMessage
	if err := json.Unmarshal(args, &pos); err != nil || len(pos) == 0 {
		return nil, errors.New(c.describeUsage())
	}
	var target string
	if err := json.Unmarshal(pos[0], &target); err != nil {
		return nil, errors.New("describe: target must be a string")
	}
	out, err := c.Describe(target)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// describeUsage names example targets: a member, one entry per group, and
// the namespaces.
func (c *Catalog) describeUsage() string {
	var paths []string
	if len(c.members) > 0 {
		paths = append(paths, fmt.Sprintf("%q", c.opts.Namespace+"."+c.members[0].Name))
	}
	namespaces := []string{fmt.Sprintf("%q", c.opts.Namespace)}
	for _, g := range c.groups {
		paths = append(paths, fmt.Sprintf("%q", g.example))
		namespaces = append(namespaces, fmt.Sprintf("%q", g.name))
	}
	if len(paths) == 0 {
		return "describe(target) needs a path or a namespace: " + strings.Join(namespaces, ", ")
	}
	return "describe(target) needs a path such as " + strings.Join(paths, ", ") + ", or a namespace: " + strings.Join(namespaces, ", ")
}

// GlobalsDeclaration is the TypeScript declaration of the search and
// describe globals.
func GlobalsDeclaration() string {
	return `/** Find tools and other catalogue entries by keywords; results are ranked, best first. */
declare function search(query: string, opts?: { limit?: number }): Promise<{
  results: { path: string; kind: string; description: string; score: number }[];
  total: number;
  truncated: boolean;
  warning?: string;
  hint?: string;
}>;
/** Describe a path ("ns.tool", "api.method"), a group ("api"), or the namespace: TypeScript types and how to call it. */
declare function describe(path: string): Promise<{
  path: string;
  kind: string;
  description?: string;
  types?: string;
  call?: string;
  members?: string[];
  hint?: string;
  warning?: string;
  [extra: string]: unknown;
}>;`
}

// normalize decodes a schema of any Go type into plain JSON values
// (map[string]any for objects). json.RawMessage, []byte and string are taken
// as raw JSON: empty means no schema, and JSON that does not parse becomes the
// empty schema, which renders as unknown. Other values that do not marshal,
// and typed nils, become nil.
func normalize(schema any) any {
	var raw []byte
	switch s := schema.(type) {
	case nil:
		return nil
	case json.RawMessage:
		raw = s
	case []byte:
		raw = s
	case string:
		raw = []byte(s)
	default:
		b, err := json.Marshal(schema)
		if err != nil {
			return nil
		}
		raw = b
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return Schema{}
	}
	return v
}

// cloneExtra copies an Extra map with cloneValue; nil stays nil.
func cloneExtra(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneValue deep-copies the JSON-like containers in v (map[string]any,
// []any, []string, map[string]string); other values are returned as they are.
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneExtra(x)
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	case []string:
		if x == nil {
			return x
		}
		return append([]string{}, x...)
	case map[string]string:
		if x == nil {
			return x
		}
		out := make(map[string]string, len(x))
		for k, s := range x {
			out[k] = s
		}
		return out
	}
	return v
}

// searchText is everything an entry is matched on: path, description,
// keywords, and the property names, descriptions and enum values of Input.
func searchText(e Entry) string {
	return strings.Join([]string{e.Path, e.Description, e.Keywords, schemaWords(e.Input)}, " ")
}

// schemaWords collects the schema description and its properties' names,
// descriptions and enum values.
func schemaWords(schema any) string {
	m, ok := schema.(map[string]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	if d, ok := m["description"].(string); ok {
		b.WriteString(d + " ")
	}
	props, _ := m["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(name + " ")
		p, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		if d, ok := p["description"].(string); ok {
			b.WriteString(d + " ")
		}
		if enum, ok := p["enum"].([]any); ok {
			for _, v := range enum {
				fmt.Fprintf(&b, "%v ", v)
			}
		}
	}
	return b.String()
}

// firstSentence returns s up to the first sentence end or paragraph break,
// with whitespace collapsed.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	end := len(s)
	for _, sep := range []string{". ", ".\n"} {
		if i := strings.Index(s, sep); i > 0 && i+1 < end {
			end = i + 1
		}
	}
	if i := strings.Index(s, "\n\n"); i > 0 && i < end {
		end = i
	}
	return strings.Join(strings.Fields(s[:end]), " ")
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
