// Command mcpserver is a minimal MCP server over stdio with two ordinary
// tools and a "code" tool that scripts them. Point an MCP client at it:
//
//	go build -o /tmp/notes ./examples/mcpserver
//	claude mcp add notes /tmp/notes          # or any MCP client
//
// Then ask the model to "add three notes and return the longest one"; it
// writes one script instead of making four tool calls.
//
// The -codemode flag takes on (default), only, or off.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	codemode "github.com/thehumanworks/codemode-go"
	"github.com/thehumanworks/codemode-go/mcptool"
)

type store struct {
	mu    sync.Mutex
	notes map[string]string
}

type addNoteIn struct {
	Title string `json:"title" jsonschema:"unique note title"`
	Body  string `json:"body" jsonschema:"note text"`
}

type listNotesOut struct {
	Notes []string `json:"notes"`
}

func main() {
	modeFlag := flag.String("codemode", "on", "code mode: on, only, or off")
	flag.Parse()
	mode, err := mcptool.ParseMode(*modeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	st := &store{notes: map[string]string{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "notes", Version: "0.1.0"}, nil)
	reg := mcptool.New(srv, mcptool.Options{
		Namespace:   "notes",
		StripPrefix: "notes_",
		Mode:        mode,
		TypesURI:    "notes://code/types",
	})

	mcptool.AddTool(reg, &mcp.Tool{Name: "notes_add", Description: "Store a note under a title."}, func(_ context.Context, _ *mcp.CallToolRequest, in addNoteIn) (*mcp.CallToolResult, map[string]any, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if _, exists := st.notes[in.Title]; exists {
			return nil, nil, fmt.Errorf("a note titled %q already exists", in.Title)
		}
		st.notes[in.Title] = in.Body
		return nil, map[string]any{"stored": in.Title, "count": len(st.notes)}, nil
	})

	mcptool.AddTool(reg, &mcp.Tool{Name: "notes_list", Description: "List note titles."}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, listNotesOut, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		titles := make([]string, 0, len(st.notes))
		for t := range st.notes {
			titles = append(titles, t)
		}
		sort.Strings(titles)
		return nil, listNotesOut{Notes: titles}, nil
	})

	mcptool.AddTool(reg, &mcp.Tool{Name: "notes_get", Description: "Read one note by title."}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Title string `json:"title"`
	}) (*mcp.CallToolResult, any, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		body, ok := st.notes[in.Title]
		if !ok {
			return nil, nil, fmt.Errorf("no note titled %q", in.Title)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
	})

	reg.Register()
	go codemode.Warm()

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
