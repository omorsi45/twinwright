// Package exectest is the agent side of the exec protocol, for tests.
//
// It is a plain package rather than a _test file so that the test binaries of
// several packages can re-execute themselves as the agent process: the package
// that runs the exec provider (internal/agent) cannot import internal/replay,
// which imports it, yet the end-to-end test that needs replay must drive the
// same agent. It deliberately does not import internal/agent either. The
// protocol is its contract, so it decodes a request with its own minimal types,
// and a drift between the two sides shows up as a failing test instead of being
// shared by construction.
//
// A test binary opts in with a TestMain that calls MaybeRun before m.Run. The
// provider under test then starts os.Executable() with Mode in its environment.
package exectest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Mode is the environment variable that turns a test binary into an agent.
// Its value names the behaviour: see Run.
const Mode = "TW_EXEC_AGENT_HELPER"

// PIDFile names a file the hanging and flooding modes write their process ID
// to, so a test can prove the process is gone once the provider returns.
const PIDFile = "TW_EXEC_PIDFILE"

// ChildSuffix is appended to PIDFile for the pid file of a process the agent started.
const ChildSuffix = ".child"

// Secret is read by the modes that echo it, so a test can prove it is masked.
const Secret = "TW_TEST_SECRET"

const protocol = "twinwright.agent.v1"

// MaybeRun runs the agent and exits when Mode is set, and returns otherwise.
func MaybeRun() {
	if mode := os.Getenv(Mode); mode != "" {
		os.Exit(Run(mode, os.Stdin, os.Stdout, os.Stderr))
	}
}

// fixed holds the modes whose whole behaviour is one fixed stdout document.
var fixed = map[string]string{
	"non-json":       "this is not json",
	"empty":          "",
	"unknown-field":  `{"protocol":"` + protocol + `","content":"hi","tool_calls":[],"surprise":true}`,
	"wrong-protocol": `{"protocol":"twinwright.agent.v2","content":"hi","tool_calls":[]}`,
	"no-protocol":    `{"content":"hi","tool_calls":[]}`,
	"no-tool-calls":  `{"protocol":"` + protocol + `","content":"hi"}`,
	"no-content":     `{"protocol":"` + protocol + `","tool_calls":[]}`,
	"nothing-to-say": `{"protocol":"` + protocol + `","content":"","tool_calls":[]}`,
	"trailing":       `{"protocol":"` + protocol + `","content":"hi","tool_calls":[]}{"again":1}`,
	// ignore-stdin answers without reading the request, which is what an agent
	// that does not care about its input looks like to the provider.
	"ignore-stdin": `{"protocol":"` + protocol + `","content":"done","tool_calls":[]}`,
	"with-usage":   `{"protocol":"` + protocol + `","content":"counted","tool_calls":[],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`,
}

// Run speaks as an agent in the named mode and returns the process exit code.
//
// Modes: billing solves the duplicate-charge scenario from the history it is
// sent; echo-request answers with the request it received as its content;
// environ answers with the sorted names of its environment; leak answers with
// the value of Secret; exit3 fails with a message on stderr; stderr-flood fails
// after writing far more than the provider keeps; flood writes stdout until it
// is stopped; hang and hang-ignore-term never answer; spawn-hang hangs with a
// child that ignores SIGTERM; every key of fixed is a malformed or odd
// document.
func Run(mode string, stdin io.Reader, stdout, stderr io.Writer) int {
	if document, ok := fixed[mode]; ok {
		io.WriteString(stdout, document)
		return 0
	}
	switch mode {
	case "billing":
		raw, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "read request:", err)
			return 2
		}
		answer, err := Respond(raw)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		stdout.Write(answer)
		return 0
	case "echo-request":
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return 2
		}
		return say(stdout, string(raw))
	case "environ":
		io.Copy(io.Discard, stdin)
		var names []string
		for _, pair := range os.Environ() {
			name, _, _ := strings.Cut(pair, "=")
			names = append(names, name)
		}
		sort.Strings(names)
		return say(stdout, strings.Join(names, ","))
	case "leak":
		io.Copy(io.Discard, stdin)
		return say(stdout, "the secret is "+os.Getenv(Secret))
	case "exit3":
		fmt.Fprintf(stderr, "agent exploded: %s\n", os.Getenv(Secret))
		return 3
	case "stderr-flood":
		line := strings.Repeat("e", 1023) + "\n"
		for range 1024 {
			io.WriteString(stderr, line)
		}
		io.WriteString(stderr, "END-OF-STDERR\n")
		return 3
	case "flood":
		writePID()
		chunk := []byte(strings.Repeat("x", 64<<10))
		for {
			if _, err := stdout.Write(chunk); err != nil {
				return 1
			}
		}
	case "hang":
		writePID()
		select {}
	case "hang-ignore-term":
		signal.Ignore(syscall.SIGTERM)
		writePID()
		select {}
	case "spawn-hang":
		// A leader that dies on SIGTERM and leaves behind a child that ignores
		// it and holds the output pipes: only a signal to the whole group, and
		// then SIGKILL, ends both.
		exe, err := os.Executable()
		if err != nil {
			return 1
		}
		child := exec.Command(exe)
		child.Env = append(os.Environ(), Mode+"=hang-ignore-term", PIDFile+"="+os.Getenv(PIDFile)+ChildSuffix)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return 1
		}
		writePID()
		select {}
	}
	fmt.Fprintf(stderr, "unknown agent mode %q\n", mode)
	return 64
}

// say writes a final answer with no tool calls.
func say(stdout io.Writer, content string) int {
	out, err := json.Marshal(response{Protocol: protocol, Content: content, ToolCalls: []toolCall{}})
	if err != nil {
		return 2
	}
	stdout.Write(out)
	return 0
}

func writePID() {
	if path := os.Getenv(PIDFile); path != "" {
		os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
}

type request struct {
	Protocol string    `json:"protocol"`
	Task     string    `json:"task"`
	History  []message `json:"history"`
}

type message struct {
	Role        string `json:"role"`
	Content     string `json:"content"`
	CallID      string `json:"call_id"`
	OperationID string `json:"operation_id"`
	Status      int    `json:"status"`
}

type response struct {
	Protocol  string     `json:"protocol"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls"`
}

type toolCall struct {
	ID          string         `json:"id"`
	OperationID string         `json:"operation_id"`
	Arguments   map[string]any `json:"arguments"`
}

// Respond answers one request document with one response document, deciding
// from the tool results in the history alone. It is the duplicate-charge
// sequence of the scripted fixture, written again against the wire format:
// read the customer, their invoices and the invoice's charges (once more if the
// charge listing came back 503), refund the second charge, and report.
func Respond(raw []byte) ([]byte, error) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	if req.Protocol != protocol {
		return nil, fmt.Errorf("unsupported protocol %q", req.Protocol)
	}
	var tools []message
	for _, m := range req.History {
		if m.Role == "tool" {
			tools = append(tools, m)
		}
	}
	answer, err := decide(tools)
	if err != nil {
		return nil, err
	}
	return json.Marshal(answer)
}

func decide(tools []message) (response, error) {
	id := "exec-" + strconv.Itoa(len(tools)+1)
	call := func(operation string, arguments map[string]any) (response, error) {
		return response{Protocol: protocol, ToolCalls: []toolCall{{ID: id, OperationID: operation, Arguments: arguments}}}, nil
	}
	switch len(tools) {
	case 0:
		return call("getCustomer", map[string]any{"id": "C-104"})
	case 1:
		return call("listInvoices", map[string]any{"id": "C-104"})
	case 2:
		return call("listCharges", map[string]any{"id": "INV-104"})
	}
	last := tools[len(tools)-1]
	switch {
	case last.OperationID == "listCharges" && last.Status == 503:
		return call("listCharges", map[string]any{"id": "INV-104"})
	case last.OperationID == "listCharges" && last.Status == 200:
		var charges []struct {
			ID          string `json:"id"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := json.Unmarshal([]byte(last.Content), &charges); err != nil {
			return response{}, fmt.Errorf("decode charges: %w", err)
		}
		if len(charges) != 2 {
			return response{}, fmt.Errorf("expected two charges on the invoice, got %d", len(charges))
		}
		return call("createRefund", map[string]any{"charge_id": charges[1].ID, "amount_cents": charges[1].AmountCents, "reason": "duplicate charge"})
	case last.OperationID == "createRefund" && last.Status == 201:
		return response{Protocol: protocol, Content: "Duplicate charge refunded.", ToolCalls: []toolCall{}}, nil
	}
	return response{}, fmt.Errorf("cannot continue after %s HTTP %d", last.OperationID, last.Status)
}
