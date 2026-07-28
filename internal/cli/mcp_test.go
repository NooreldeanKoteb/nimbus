package cli

import (
	"context"

	"github.com/nkoteb/nimbus/internal/config"
	"github.com/nkoteb/nimbus/internal/device"
	"strings"
	"testing"
)

// call invokes an MCP tool against this device's real state.
func (d *fakeDevice) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	d.t.Setenv("NIMBUS_HOME", d.home)
	// Same identity the CLI path uses. Without this a tool call would resolve
	// to the host's real machine id and read a different device's policy and
	// claims than `run` just wrote.
	d.t.Setenv(device.NodeIDEnv, d.id)

	// Attended is left false: a tool call is Claude acting, not a person, which
	// is the distinction the autonomy ladder is measured against.
	env := &Env{Out: nil, Err: nil}
	paths, err := resolvePathsForTest()
	if err != nil {
		t.Fatal(err)
	}
	env.Paths = paths

	for _, tool := range tools(env) {
		if tool.Name != name {
			continue
		}
		out, err := tool.Call(context.Background(), args)
		if err != nil {
			return err.Error(), false
		}
		return out, true
	}
	t.Fatalf("no such tool %q", name)
	return "", false
}

// The tools are shims over the CLI, so this is really asserting that the shim
// shapes arguments correctly and that state actually moves.
func TestMCPToolsReachRealState(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "port-server", "--goal", "port the server")

	if out, ok := d.call(t, "nimbus_memory_add", map[string]any{
		"text": "postgres listens on 5433 here", "long_term": true,
	}); !ok {
		t.Fatalf("memory_add failed: %s", out)
	}

	if out, ok := d.call(t, "nimbus_task_note", map[string]any{
		"text": "the handler is stubbed",
	}); !ok {
		t.Fatalf("task_note failed: %s", out)
	}

	// Everything written above has to show up in the brief the model reads.
	brief, ok := d.call(t, "nimbus_context", nil)
	if !ok {
		t.Fatalf("context failed: %s", brief)
	}
	for _, want := range []string{"port the server", "postgres listens on 5433", "the handler is stubbed"} {
		if !strings.Contains(brief, want) {
			t.Errorf("context is missing %q:\n%s", want, brief)
		}
	}
}

// Repeatable flags arrive as JSON arrays, and a model will sometimes send a
// bare string where a list was asked for.
func TestMCPHandoffAcceptsListsAndStrings(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "port-server", "--goal", "port it")

	if out, ok := d.call(t, "nimbus_handoff", map[string]any{
		"done":        []any{"wrote the client", "wrote the tests"},
		"next":        "wire up auth",
		"outside_git": []any{"postgres in docker on :5432"},
		"notes":       "the refresh endpoint is stubbed",
	}); !ok {
		t.Fatalf("handoff failed: %s", out)
	}

	shown := d.run("task", "show", "port-server")
	for _, want := range []string{
		"wrote the client", "wrote the tests", // list form
		"wire up auth",                // bare string form
		"postgres in docker",          // outside-git
		"refresh endpoint is stubbed", // notes, piped on stdin
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("handoff dropped %q:\n%s", want, shown)
		}
	}
}

// A tool failure has to come back as readable text, not as a crash: the model
// is supposed to read it and try something else.
func TestMCPToolErrorsAreReadable(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out, ok := d.call(t, "nimbus_send", map[string]any{"to": "ghost-device", "text": "hello"})
	if ok {
		t.Fatal("sending to a nonexistent device succeeded")
	}
	if !strings.Contains(out, "device") {
		t.Errorf("error is not actionable: %q", out)
	}

	// Missing required arguments must be named, not silently defaulted.
	out, ok = d.call(t, "nimbus_send", map[string]any{"text": "no recipient"})
	if ok {
		t.Fatal("send succeeded with no recipient")
	}
	if !strings.Contains(out, "to") {
		t.Errorf("error does not name the missing argument: %q", out)
	}
}

// Every tool must be callable with only its required arguments present.
func TestEveryToolHasAWorkingSchema(t *testing.T) {
	env := &Env{}
	for _, tool := range tools(env) {
		if tool.Description == "" {
			t.Errorf("tool %q has no description; the model has nothing to go on", tool.Name)
		}
		if tool.Call == nil {
			t.Errorf("tool %q has no implementation", tool.Name)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %q schema type = %q, want object", tool.Name, tool.InputSchema.Type)
		}
		// Anything listed as required must actually be described.
		for _, req := range tool.InputSchema.Required {
			if _, ok := tool.InputSchema.Properties[req]; !ok {
				t.Errorf("tool %q requires %q but does not describe it", tool.Name, req)
			}
		}
	}
}

// resolvePathsForTest resolves paths the same way a real command does, so the
// tool test exercises the real NIMBUS_HOME plumbing.
func resolvePathsForTest() (*config.Paths, error) {
	return config.Resolve()
}
