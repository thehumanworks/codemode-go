package catalog

import "testing"

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
