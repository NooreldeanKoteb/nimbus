// Package mcp implements enough of the Model Context Protocol for Claude Code
// to call nimbus as a tool server.
//
// Hand-rolled over encoding/json rather than pulled from a library: MCP over
// stdio is JSON-RPC 2.0 with three methods that matter, and §2a's whole point
// is that nimbus stays a single dependency-free binary. An SDK here would cost
// more than it saves.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the MCP revision this server implements.
const ProtocolVersion = "2024-11-05"

// JSON-RPC error codes used here.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// request is an incoming JSON-RPC message.
//
// ID is json.RawMessage because the spec allows a string or a number and it
// must be echoed back in exactly the form it arrived. Decoding it into a
// concrete type would turn 1 into 1.0 and break correlation on some clients.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool is one callable exposed to the model.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// InputSchema is JSON Schema. The model chooses arguments from this alone,
	// so it carries the real documentation.
	InputSchema Schema `json:"inputSchema"`

	// Call runs the tool. Returning an error surfaces it to the model as tool
	// output rather than as a protocol failure, so the model can read the
	// message and try something else.
	Call func(ctx context.Context, args map[string]any) (string, error) `json:"-"`
}

// Schema is a minimal JSON Schema object.
type Schema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

// Property describes one argument.
type Property struct {
	Type        string    `json:"type"`
	Description string    `json:"description,omitempty"`
	Items       *Property `json:"items,omitempty"`
}

// ObjectSchema builds a schema for a tool taking named arguments.
func ObjectSchema(props map[string]Property, required ...string) Schema {
	return Schema{Type: "object", Properties: props, Required: required}
}

// Server serves MCP over a reader/writer pair, normally stdin and stdout.
type Server struct {
	Name    string
	Version string
	Tools   []Tool

	mu  sync.Mutex
	out *json.Encoder
}

// Serve reads requests until the stream closes or the context is cancelled.
//
// stdout carries protocol only. Anything a human should see has to go to
// stderr, because a stray print here corrupts the JSON stream and the client
// disconnects with a parse error that names nothing useful.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)
	scanner := bufio.NewScanner(in)
	// Tool results carry briefs and message bodies, so lines get long.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.send(response{JSONRPC: "2.0", Error: &rpcError{CodeParseError, "invalid JSON"}})
			continue
		}
		s.handle(ctx, req)
	}
	return scanner.Err()
}

func (s *Server) handle(ctx context.Context, req request) {
	// Notifications have no id and must never be answered; replying to one is
	// a protocol violation that some clients treat as fatal.
	notification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		s.reply(req, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		})

	case "notifications/initialized", "notifications/cancelled":
		// Nothing to do, and nothing to say.

	case "tools/list":
		s.reply(req, map[string]any{"tools": s.Tools})

	case "tools/call":
		s.callTool(ctx, req)

	case "ping":
		s.reply(req, map[string]any{})

	default:
		if !notification {
			s.fail(req, CodeMethodNotFound, "unknown method "+req.Method)
		}
	}
}

func (s *Server) callTool(ctx context.Context, req request) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.fail(req, CodeInvalidParams, "bad tool call parameters")
		return
	}

	for _, tool := range s.Tools {
		if tool.Name != params.Name {
			continue
		}
		output, err := tool.Call(ctx, params.Arguments)
		if err != nil {
			// A failed tool is a normal outcome the model should see and react
			// to, not a transport error that kills the request.
			s.reply(req, toolResult(err.Error(), true))
			return
		}
		s.reply(req, toolResult(output, false))
		return
	}
	s.fail(req, CodeInvalidParams, "unknown tool "+params.Name)
}

func toolResult(text string, isError bool) map[string]any {
	if text == "" {
		text = "(no output)"
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (s *Server) reply(req request, result any) {
	if len(req.ID) == 0 {
		return
	}
	s.send(response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) fail(req request, code int, message string) {
	if len(req.ID) == 0 {
		return
	}
	s.send(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{code, message}})
}

func (s *Server) send(r response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.out.Encode(r); err != nil {
		// Nothing useful to do: the client is gone or stdout is broken, and
		// writing an error about a failed write would go to the same place.
		_ = err
	}
}

// StringArg reads a string argument, tolerating a missing key.
func StringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// BoolArg reads a boolean argument.
func BoolArg(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

// StringsArg reads a list-of-strings argument. JSON gives []any, and a model
// sometimes sends a bare string where a list was asked for, so both are taken.
func StringsArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// RequireArg reads a mandatory string argument.
func RequireArg(args map[string]any, key string) (string, error) {
	if v := StringArg(args, key); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("missing required argument %q", key)
}
