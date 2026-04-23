package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoArgs struct {
	Msg string `json:"msg"`
}

// newEchoServerSession spins up an in-memory MCP server with a single "echo"
// tool that returns its msg argument wrapped in TextContent, and returns a
// connected client session. All resources are cleaned up via t.Cleanup.
func newEchoServerSession(t *testing.T, ctx context.Context) *mcpsdk.ClientSession {
	t.Helper()

	ct, st := mcpsdk.NewInMemoryTransports()

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "echo",
		Description: "echo the msg argument back as text",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + args.Msg}},
		}, nil, nil
	})

	serverSession, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

func TestToolAdapter_InMemory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		serverName string
		args       string
		wantInText string
	}{
		{
			name:       "echo tool round-trips through adapter",
			serverName: "echo-srv",
			args:       `{"msg":"hi"}`,
			wantInText: "echo: hi",
		},
		{
			name:       "namespaced server name is sanitized",
			serverName: "with spaces",
			args:       `{"msg":"ok"}`,
			wantInText: "echo: ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			session := newEchoServerSession(t, ctx)

			list, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			if len(list.Tools) != 1 {
				t.Fatalf("len(Tools) = %d, want 1", len(list.Tools))
			}
			if list.Tools[0].Name != "echo" {
				t.Fatalf("Tools[0].Name = %q, want %q", list.Tools[0].Name, "echo")
			}

			adapter := &ToolAdapter{
				serverName: tt.serverName,
				toolName:   list.Tools[0].Name,
				tool:       list.Tools[0],
				session:    session,
			}

			wantName := ToolName(tt.serverName, "echo")
			if got := adapter.Name(); got != wantName {
				t.Errorf("Name() = %q, want %q", got, wantName)
			}

			raw, err := adapter.Run(ctx, nil, []byte(tt.args))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			var got mcpsdk.CallToolResult
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal result: %v\nraw: %s", err, raw)
			}
			if got.IsError {
				t.Fatalf("result.IsError = true, want false; content=%+v", got.Content)
			}
			if len(got.Content) == 0 {
				t.Fatalf("result has no Content; raw: %s", raw)
			}

			tc, ok := got.Content[0].(*mcpsdk.TextContent)
			if !ok {
				t.Fatalf("Content[0] is %T, want *TextContent", got.Content[0])
			}
			if tc.Text != tt.wantInText {
				t.Errorf("TextContent.Text = %q, want %q", tc.Text, tt.wantInText)
			}
		})
	}
}

func TestToolAdapter_Usage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session := newEchoServerSession(t, ctx)

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	adapter := &ToolAdapter{
		serverName: "srv",
		toolName:   list.Tools[0].Name,
		tool:       list.Tools[0],
		session:    session,
	}

	usage := adapter.Usage()
	if usage.Name != ToolName("srv", "echo") {
		t.Errorf("Usage.Name = %q, want %q", usage.Name, ToolName("srv", "echo"))
	}
	if usage.Parameters == nil {
		t.Error("Usage.Parameters is nil")
	}
	if typ, _ := usage.Parameters["type"].(string); typ != "object" {
		t.Errorf(`Usage.Parameters["type"] = %v, want "object"`, usage.Parameters["type"])
	}
}

