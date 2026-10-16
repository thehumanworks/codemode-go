package codemode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	codemode "github.com/thehumanworks/codemode-go"
)

// Two ordinary Go functions become methods a script can call.
func ExampleRun() {
	tools := []codemode.Tool{
		{Name: "list_users", JSName: "listUsers", Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`[{"name":"ada","admin":true},{"name":"bob","admin":false}]`), nil
		}},
		{Name: "greet", JSName: "greet", Call: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var in struct{ Name string }
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			return json.Marshal("hello " + strings.ToUpper(in.Name))
		}},
	}

	script := `
		const users = await app.listUsers();
		const admins = users.filter(u => u.admin);
		console.log("admins:", admins.length);
		return Promise.all(admins.map(u => app.greet({name: u.name})));
	`
	out, err := codemode.Run(context.Background(), script, tools, nil, codemode.Options{Namespace: "app"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(string(out.Result))
	fmt.Println(out.Logs[0], "| calls:", out.Calls)
	// Output:
	// ["hello ADA"]
	// admins: 1 | calls: 2
}
