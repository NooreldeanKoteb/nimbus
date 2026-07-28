package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// exchange feeds requests through a server and returns the decoded replies.
func exchange(t *testing.T, s *Server, requests ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")

	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var reply map[string]any
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			t.Fatalf("server emitted non-JSON on stdout: %q", line)
		}
		replies = append(replies, reply)
	}
	return replies
}

func testServer(tools ...Tool) *Server {
	return &Server{Name: "nimbus", Version: "test", Tools: tools}
}

func echoTool() Tool {
	return Tool{
		Name:        "echo",
		Description: "echo back",
		InputSchema: ObjectSchema(map[string]Property{"text": {Type: "string"}}, "text"),
		Call: func(_ context.Context, args map[string]any) (string, error) {
			return StringArg(args, "text"), nil
		},
	}
}

func TestInitializeHandshake(t *testing.T) {
	replies := exchange(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	result := replies[0]["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", result["protocolVersion"], ProtocolVersion)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("server did not advertise tool support")
	}
}

// Replying to a notification is a protocol violation some clients treat as
// fatal, and notifications are exactly what a client sends right after
// initialize.
func TestNotificationsGetNoReply(t *testing.T) {
	replies := exchange(t, testServer(echoTool()),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled"}`,
		`{"jsonrpc":"2.0","method":"some/unknown/notification"}`)

	if len(replies) != 0 {
		t.Errorf("got %d replies to notifications, want 0: %v", len(replies), replies)
	}
}

func TestToolsList(t *testing.T) {
	replies := exchange(t, testServer(echoTool()),
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	tools := replies[0]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "echo" {
		t.Errorf("name = %v", tool["name"])
	}
	// The model picks arguments from the schema alone, so it has to be there.
	schema := tool["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	if req := schema["required"].([]any); len(req) != 1 || req[0] != "text" {
		t.Errorf("required = %v, want [text]", req)
	}
}

func TestToolCall(t *testing.T) {
	replies := exchange(t, testServer(echoTool()),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}`)

	result := replies[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Errorf("isError = %v, want false", result["isError"])
	}
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "hello" {
		t.Errorf("text = %v, want hello", content["text"])
	}
}

// A failing tool is a normal outcome the model should read and react to, not a
// transport error that kills the request.
func TestFailingToolIsReportedAsContent(t *testing.T) {
	failing := Tool{
		Name: "boom", InputSchema: ObjectSchema(nil),
		Call: func(context.Context, map[string]any) (string, error) {
			return "", errors.New("device is offline")
		},
	}
	replies := exchange(t, testServer(failing),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom","arguments":{}}}`)

	if _, isRPCError := replies[0]["error"]; isRPCError {
		t.Fatal("a tool failure was reported as a protocol error")
	}
	result := replies[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Error("isError = false for a failing tool")
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "device is offline") {
		t.Errorf("the model cannot see why it failed: %v", content["text"])
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	replies := exchange(t, testServer(echoTool()),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"no/such/method"}`)

	for i, reply := range replies {
		if _, ok := reply["error"]; !ok {
			t.Errorf("reply %d has no error object: %v", i, reply)
		}
	}
}

func TestMalformedInputDoesNotKillTheServer(t *testing.T) {
	replies := exchange(t, testServer(echoTool()),
		`{ not json at all`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"still here"}}}`)

	if len(replies) != 2 {
		t.Fatalf("got %d replies, want a parse error then a working call", len(replies))
	}
	result := replies[1]["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "still here" {
		t.Errorf("server did not recover from bad input: %v", content["text"])
	}
}

// The id must come back exactly as it arrived: decoding into a concrete type
// would turn 1 into 1.0 and break correlation on strict clients.
func TestRequestIDIsEchoedVerbatim(t *testing.T) {
	replies := exchange(t, testServer(),
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"abc","method":"ping"}`)

	if got := replies[0]["id"]; got != float64(7) {
		t.Errorf("numeric id came back as %#v", got)
	}
	if got := replies[1]["id"]; got != "abc" {
		t.Errorf("string id came back as %#v", got)
	}
}

func TestArgumentCoercion(t *testing.T) {
	args := map[string]any{
		"str":    "value",
		"yes":    true,
		"list":   []any{"a", "b", ""},
		"single": "solo",
		"number": 42.0,
	}

	if got := StringArg(args, "str"); got != "value" {
		t.Errorf("StringArg = %q", got)
	}
	if got := StringArg(args, "number"); got != "" {
		t.Errorf("StringArg on a number = %q, want empty", got)
	}
	if !BoolArg(args, "yes") || BoolArg(args, "missing") {
		t.Error("BoolArg is wrong")
	}

	if got := StringsArg(args, "list"); len(got) != 2 {
		t.Errorf("StringsArg = %v, want the empty entry dropped", got)
	}
	// Models routinely send a bare string where a list was asked for.
	if got := StringsArg(args, "single"); len(got) != 1 || got[0] != "solo" {
		t.Errorf("StringsArg on a bare string = %v", got)
	}
	if got := StringsArg(args, "missing"); got != nil {
		t.Errorf("StringsArg on a missing key = %v", got)
	}

	if _, err := RequireArg(args, "missing"); err == nil {
		t.Error("RequireArg accepted a missing argument")
	}
}
