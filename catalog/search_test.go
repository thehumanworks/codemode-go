package catalog

import (
	"reflect"
	"testing"
)

func TestSearch(t *testing.T) {
	idx := NewIndex([]Document{
		{Path: "rex.capture", Text: "rex_capture Read a pane's screen. Return the current screen contents of a terminal pane plus process state."},
		{Path: "rex.send", Text: "rex_send Type into a pane. Send text and key presses to a terminal pane."},
		{Path: "api.session.new_split", Text: "session.new_split Split a pane: create a new block beside an existing one in the same window."},
		{Path: "api.session.resize_pane", Text: "session.resize_pane Resize a split by moving its border."},
		{Path: "rex.client", Text: "rex_client Control a connected Rex app: font size, sidebar, tabs."},
	})
	first := func(q string) string {
		m := idx.Search(q, 3)
		if len(m) == 0 {
			return ""
		}
		return m[0].Path
	}
	if got := first("read screen of pane"); got != "rex.capture" {
		t.Errorf("screen: %s", got)
	}
	if got := first("type keys"); got != "rex.send" {
		t.Errorf("keys: %s", got)
	}
	if got := first("resize splits"); got != "api.session.resize_pane" {
		t.Errorf("resize: %s", got)
	}
	if got := first("fontSize"); got != "rex.client" {
		t.Errorf("camel: %s", got)
	}
	if m := idx.Search("the and of", 5); len(m) != 0 {
		t.Errorf("stop words only should match nothing: %v", m)
	}
	if m := idx.Search("pane", 2); len(m) != 2 {
		t.Errorf("limit: %d", len(m))
	}
}

func TestTokenize(t *testing.T) {
	cases := map[string][]string{
		"List block IDs.": {"list", "block", "id"},
		"blockIDs":        {"block", "id"},
		"ids":             {"id"},
		"URLs":            {"url"},
		"APIs":            {"api"},
		"UUIDs":           {"uuid"},
		"getURLsForPage":  {"get", "url", "page"},
		"IDsList":         {"id", "list"},
		"IDs_list":        {"id", "list"},
		"HTTPServer":      {"http", "server"},
		"fontSize":        {"font", "size"},
		"rex_capture":     {"rex", "capture"},
		"panes":           {"pane"},
		"画面を読む":           {"画面を読む"},
		"café crème":      {"café", "crème"},
		"Ünïcode Wörter":  {"ünïcode", "wörter"},
		"nai\u0308ve":     {"nai\u0308ve"}, // combining mark stays in the word
		"résumés, 2 ÉTÉS": {"résumé", "2", "été"},
	}
	for in, want := range cases {
		if got := Tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchPluralAcronymsAndNonASCII(t *testing.T) {
	idx := NewIndex([]Document{
		{Path: "rex.blocks", Text: "rex.blocks List block IDs. blockIDs"},
		{Path: "rex.open", Text: "rex.open Open URLs in a browser tab."},
		{Path: "jp.read", Text: "jp.read 画面を読む"},
		{Path: "fr.menu", Text: "fr.menu La carte du café"},
		{Path: "rex.send", Text: "rex.send Type into a pane."},
	})
	for q, want := range map[string]string{
		"ids":       "rex.blocks",
		"block IDs": "rex.blocks",
		"url":       "rex.open",
		"URLs":      "rex.open",
		"画面を読む":     "jp.read",
		"café":      "fr.menu",
	} {
		m := idx.Search(q, 3)
		if len(m) == 0 || m[0].Path != want {
			t.Errorf("Search(%q) = %v, want %s first", q, m, want)
		}
	}
}
