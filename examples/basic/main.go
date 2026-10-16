// Command basic runs a script from stdin against three Go functions, with no
// MCP involved. Try:
//
//	echo 'const n = await app.wordCount({text: "a b c"}); return n * (await app.add({a: 2, b: 3}))' | go run ./examples/basic
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	codemode "github.com/thehumanworks/codemode-go"
)

func main() {
	code, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	tools := []codemode.Tool{
		{Name: "add", Call: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			var in struct{ A, B float64 }
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			return json.Marshal(in.A + in.B)
		}},
		{Name: "word_count", JSName: "wordCount", Call: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			var in struct{ Text string }
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			return json.Marshal(len(strings.Fields(in.Text)))
		}},
		{Name: "now", Call: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(time.Now().UTC().Format(time.RFC3339))
		}},
	}

	runner := codemode.NewRunner(tools, nil, codemode.Options{
		Namespace: "app",
		Timeout:   10 * time.Second,
		OnLog:     func(line string) { fmt.Fprintln(os.Stderr, "console:", line) },
	})
	out, err := runner.Run(context.Background(), string(code))

	var scriptErr *codemode.ScriptError
	switch {
	case err == nil:
		fmt.Printf("%s\n(%d calls, %s)\n", out.Result, out.Calls, out.Elapsed.Round(time.Millisecond))
	case errors.As(err, &scriptErr):
		fmt.Fprintln(os.Stderr, "script failed:", scriptErr.Message)
		os.Exit(1)
	case errors.Is(err, codemode.ErrTimeout):
		fmt.Fprintln(os.Stderr, "script timed out")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
