package catalog

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Document is one searchable entry: a tool, an API method, or anything else
// a script may want to find. Text is everything worth matching on.
type Document struct {
	Path string
	Text string
}

// Match is a ranked search hit.
type Match struct {
	Path  string
	Score float64
}

var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"for": true, "from": true, "in": true, "is": true, "it": true, "of": true, "on": true, "or": true,
	"that": true, "the": true, "this": true, "to": true, "with": true,
}

var (
	camelRe    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	acronymRe  = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)
)

// stem is a naive singulariser so "panes" matches "pane".
func stem(t string) string {
	switch {
	case len(t) > 4 && strings.HasSuffix(t, "ies"):
		return t[:len(t)-3] + "y"
	case len(t) > 4 && (strings.HasSuffix(t, "ches") || strings.HasSuffix(t, "shes") || strings.HasSuffix(t, "sses") || strings.HasSuffix(t, "xes") || strings.HasSuffix(t, "zes")):
		return t[:len(t)-2]
	case len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss"):
		return t[:len(t)-1]
	}
	return t
}

// Tokenize lowercases, splits camelCase and punctuation, drops stop words,
// and stems.
func Tokenize(text string) []string {
	text = camelRe.ReplaceAllString(text, "$1 $2")
	text = acronymRe.ReplaceAllString(text, "$1 $2")
	text = strings.ToLower(text)
	var out []string
	for _, t := range nonAlnumRe.Split(text, -1) {
		if t == "" || stopWords[t] {
			continue
		}
		out = append(out, stem(t))
	}
	return out
}

// Index is a prepared BM25 index over documents.
type Index struct {
	docs   []Document
	counts []map[string]int
	lens   []int
	avgLen float64
	df     map[string]int
}

// NewIndex tokenizes the documents once.
func NewIndex(docs []Document) *Index {
	idx := &Index{docs: docs, df: map[string]int{}}
	total := 0
	for _, d := range docs {
		counts := map[string]int{}
		n := 0
		for _, t := range Tokenize(d.Text) {
			counts[t]++
			n++
		}
		for t := range counts {
			idx.df[t]++
		}
		idx.counts = append(idx.counts, counts)
		idx.lens = append(idx.lens, n)
		total += n
	}
	if len(docs) > 0 {
		idx.avgLen = float64(total) / float64(len(docs))
	}
	if idx.avgLen == 0 {
		idx.avgLen = 1
	}
	return idx
}

// Search ranks documents for the query with Okapi BM25 (k1 1.2, b 0.75).
// Exact token matches on the path get a boost so a tool named like the
// query comes first.
func (idx *Index) Search(query string, limit int) []Match {
	terms := uniq(Tokenize(query))
	if len(terms) == 0 || len(idx.docs) == 0 || limit <= 0 {
		return nil
	}
	const k1, b = 1.2, 0.75
	n := float64(len(idx.docs))
	var matches []Match
	for i, d := range idx.docs {
		score := 0.0
		pathTokens := map[string]bool{}
		for _, t := range Tokenize(d.Path) {
			pathTokens[t] = true
		}
		for _, t := range terms {
			c := idx.counts[i][t]
			if c == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(idx.df[t])+0.5)/(float64(idx.df[t])+0.5))
			norm := k1 * (1 - b + b*float64(idx.lens[i])/idx.avgLen)
			score += idf * (float64(c) * (k1 + 1) / (float64(c) + norm))
			if pathTokens[t] {
				score += 1.5
			}
		}
		if score > 0 {
			matches = append(matches, Match{Path: d.Path, Score: score})
		}
	}
	sort.SliceStable(matches, func(a, c int) bool { return matches[a].Score > matches[c].Score })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
