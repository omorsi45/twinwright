package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"twinwright/internal/compiler"
	"twinwright/internal/redact"
)

// AgentProtocol names the wire format between Twinwright and an agent process.
// Both the request and the response carry it, so a version mismatch is refused
// on either side instead of being read as a differently shaped document.
const AgentProtocol = "twinwright.agent.v1"

// MaxAgentOutputBytes is the most an agent may write to stdout in one turn.
// Past it the turn fails and the process is killed: an agent that floods its
// output must not be able to take the runner's memory with it.
const MaxAgentOutputBytes = 1 << 20

// maxAgentStderrBytes is how much of the end of the agent's stderr is kept for
// error text. The tail is the useful part of a crash report, and a bound is the
// only thing that makes an agent that logs without end harmless.
const maxAgentStderrBytes = 4 << 10

// execKillGrace is how long a timed out agent gets between SIGTERM and SIGKILL,
// and how long output pipes are waited on once a process has gone.
const execKillGrace = 500 * time.Millisecond

// ExecProvider lets a separate program make the agent's decisions.
//
// Each turn starts the program afresh, writes one JSON request to its stdin,
// closes stdin, and reads one JSON response from its stdout (docs/protocol/
// agent-v1.md). The program proposes tool calls and nothing else: the
// dispatcher still authorizes and executes them, so what a run is allowed to do
// does not depend on the agent being well behaved.
//
// The process is started once per turn so that nothing survives between turns
// except the history it is sent. That is what makes a run resumable on another
// machine, and what keeps a crashed agent from taking the run's state with it.
type ExecProvider struct {
	// Command is the argv of the agent. It is never passed to a shell.
	Command []string
	// Env holds NAME=value pairs for the child. The child environment is exactly
	// these plus PATH and HOME from this process, so provider keys, database
	// addresses and anything else in the operator's environment stay behind
	// unless they were named on purpose.
	Env []string
	// Timeout is the deadline of one turn. Zero means DefaultCallTimeout.
	Timeout time.Duration
	// Wrap, when set, rewrites the argv before it is run. Container mode uses it
	// to put the agent behind `docker run`; nil runs Command directly.
	Wrap func(argv []string) []string
}

// execRequest is the document an agent reads from stdin.
type execRequest struct {
	Protocol   string          `json:"protocol"`
	Task       string          `json:"task"`
	History    []Message       `json:"history"`
	Operations []execOperation `json:"operations"`
}

// execOperation describes one operation the agent may propose a call to.
type execOperation struct {
	OperationID string `json:"operation_id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
	// Parameters is a JSON Schema object for the call's arguments, the same one
	// the hosted-model adapters send as a tool definition.
	Parameters map[string]any `json:"parameters"`
}

// execResponse is the document an agent writes to stdout. Its required fields
// are pointers so that "absent" can be told from "empty": a missing tool_calls
// is a protocol error, while an empty list is a final answer.
type execResponse struct {
	Protocol  *string         `json:"protocol"`
	Content   *string         `json:"content"`
	ToolCalls *[]execToolCall `json:"tool_calls"`
	Usage     *Usage          `json:"usage,omitempty"`
}

// execToolCall is a proposed call, with the same presence rule as execResponse.
type execToolCall struct {
	ID          *string         `json:"id"`
	OperationID *string         `json:"operation_id"`
	Arguments   *map[string]any `json:"arguments"`
}

// newExecRequest builds the request for one turn.
func newExecRequest(task string, history []Message, ops []compiler.Operation) execRequest {
	if history == nil {
		history = []Message{}
	}
	operations := make([]execOperation, 0, len(ops))
	for _, op := range ops {
		properties := map[string]any{}
		for name, typ := range op.Properties {
			properties[name] = map[string]any{"type": typ}
		}
		required := op.Required
		if required == nil {
			required = []string{}
		}
		key := op.Behavior
		if key == "" {
			key = op.ID
		}
		operations = append(operations, execOperation{
			OperationID: op.ID, Method: op.Method, Path: op.Path, Description: description(key),
			Parameters: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
		})
	}
	return execRequest{Protocol: AgentProtocol, Task: task, History: history, Operations: operations}
}

// decodeExecResponse reads exactly one response object. An unknown field is an
// error, the same as in the ledger formats: a field the runner would silently
// drop is a field the agent author believes is doing something.
func decodeExecResponse(raw []byte, out *execResponse) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	// Numbers stay exact on their way into the transcript, as in the adapters.
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("agent response is not a valid JSON object: %w", err)
		}
		return fmt.Errorf("agent response does not follow the protocol: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("agent wrote data after the response object; the response must be exactly one JSON object")
	}
	switch {
	case out.Protocol == nil:
		return fmt.Errorf("agent response has no %q field; it must be %q", "protocol", AgentProtocol)
	case *out.Protocol != AgentProtocol:
		return fmt.Errorf("agent response protocol is %q, but this runner speaks %q", *out.Protocol, AgentProtocol)
	case out.Content == nil:
		return fmt.Errorf("agent response is missing %q (use an empty string when there is no text)", "content")
	case out.ToolCalls == nil:
		return fmt.Errorf("agent response is missing %q (use [] when there are no calls)", "tool_calls")
	}
	for i, call := range *out.ToolCalls {
		switch {
		case call.ID == nil:
			return fmt.Errorf("agent tool call %d is missing %q", i, "id")
		case call.OperationID == nil:
			return fmt.Errorf("agent tool call %d is missing %q", i, "operation_id")
		case call.Arguments == nil:
			return fmt.Errorf("agent tool call %d is missing %q (use {} when there are none)", i, "arguments")
		}
	}
	return nil
}

// Next runs the agent for one turn.
//
// Only a timeout is an interruption, so the runner's single retry covers an
// agent that was slow once. Everything else (a crash, malformed output, a
// protocol mismatch) is an ordinary provider error: asking the same program the
// same question again is not expected to change the answer, and two identical
// failures are worse evidence than one.
func (p ExecProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	if len(p.Command) == 0 || p.Command[0] == "" {
		return Message{}, errors.New("exec provider has no command to run")
	}
	env, secrets, err := p.childEnvironment()
	if err != nil {
		return Message{}, err
	}
	argv := slices.Clone(p.Command)
	if p.Wrap != nil {
		if argv = p.Wrap(argv); len(argv) == 0 {
			return Message{}, errors.New("exec provider: the command wrapper returned nothing to run")
		}
	}
	request, err := json.Marshal(newExecRequest(task, history, ops))
	if err != nil {
		return Message{}, err
	}

	deadline := callDeadline(p.Timeout)
	attemptCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	stdout := &boundedBuffer{limit: MaxAgentOutputBytes, exceeded: make(chan struct{})}
	stderr := &tailBuffer{max: maxAgentStderrBytes}
	// fail renders an error the way it may be stored: with the tail of stderr,
	// which is where an agent's own diagnosis is, and with every passed secret
	// and key-like token masked, since both end up in the ledger and the logs.
	fail := func(format string, args ...any) error {
		text := fmt.Sprintf(format, args...)
		if tail := stderr.String(); tail != "" {
			text += "; stderr (last " + strconv.Itoa(maxAgentStderrBytes) + " bytes): " + strconv.Quote(tail)
		}
		return errors.New(redact.String(text, secrets...))
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Bounds how long Wait lingers on output pipes that a process the agent
	// started is still holding after the agent itself has gone.
	cmd.WaitDelay = execKillGrace
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Message{}, fail("prepare agent: %v", err)
	}
	if err = cmd.Start(); err != nil {
		return Message{}, fail("start agent: %v", err)
	}
	// Written from a goroutine so that an agent which never reads cannot block
	// the turn on a full pipe. Its errors are the agent having exited or been
	// killed, which the wait below reports.
	go func() {
		stdin.Write(request)
		stdin.Close()
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-stdout.exceeded:
		stopProcessGroup(cmd, waited)
		return Message{}, fail("agent output exceeded %d bytes", MaxAgentOutputBytes)
	case <-attemptCtx.Done():
		stopProcessGroup(cmd, waited)
		if err = ctx.Err(); err != nil {
			return Message{}, err
		}
		return Message{}, interrupted(ctx, attemptCtx, deadline, fail("the agent did not answer"))
	}

	// An output that went over the limit just as the process ended is still an
	// overflow, whichever event the select happened to see first.
	if stdout.overflowed() {
		return Message{}, fail("agent output exceeded %d bytes", MaxAgentOutputBytes)
	}
	if waitErr != nil {
		var exit *exec.ExitError
		switch {
		case errors.As(waitErr, &exit) && exit.ExitCode() >= 0:
			return Message{}, fail("agent exited with status %d", exit.ExitCode())
		case errors.As(waitErr, &exit):
			return Message{}, fail("agent ended abnormally: %s", exit.String())
		case errors.Is(waitErr, exec.ErrWaitDelay):
			return Message{}, fail("agent exited, but a process it started kept its output open")
		}
		return Message{}, fail("wait for agent: %v", waitErr)
	}

	output := stdout.Bytes()
	if len(bytes.TrimSpace(output)) == 0 {
		return Message{}, fail("agent produced no output; it must write one JSON object to stdout")
	}
	var response execResponse
	if err = decodeExecResponse([]byte(maskSecrets(string(output), secrets)), &response); err != nil {
		return Message{}, fail("%v", err)
	}
	message := Message{Role: "assistant", Content: *response.Content, Usage: response.Usage}
	for _, call := range *response.ToolCalls {
		message.ToolCalls = append(message.ToolCalls, ToolCall{ID: *call.ID, OperationID: *call.OperationID, Arguments: *call.Arguments})
	}
	if message.Content == "" && len(message.ToolCalls) == 0 {
		return Message{}, fail("agent response has no content and no tool calls")
	}
	return message, nil
}

// childEnvironment builds the agent's environment and the values to mask.
func (p ExecProvider) childEnvironment() (env, secrets []string, err error) {
	passed := map[string]bool{}
	for _, pair := range p.Env {
		name, value, ok := strings.Cut(pair, "=")
		switch {
		case !ok:
			// No "=" means no value, so naming the entry shows nothing private.
			return nil, nil, fmt.Errorf("exec provider: environment entry %q is not NAME=value", pair)
		case name == "":
			return nil, nil, errors.New("exec provider: an environment entry has an empty name")
		}
		passed[name] = true
		secrets = append(secrets, value)
	}
	for _, name := range []string{"PATH", "HOME"} {
		if value, ok := os.LookupEnv(name); ok && !passed[name] {
			env = append(env, name+"="+value)
		}
	}
	return append(env, p.Env...), secrets, nil
}

// stopProcessGroup ends a process group that has overrun, and reaps it.
//
// SIGTERM first, so a well-behaved agent can clean up; SIGKILL after the grace
// period whatever happened, because a descendant that ignored SIGTERM may be
// holding the group open. It returns only once the child has been waited for, so
// no zombie is left behind.
func stopProcessGroup(cmd *exec.Cmd, waited <-chan error) {
	terminateProcessGroup(cmd)
	exited := false
	select {
	case <-waited:
		exited = true
	case <-time.After(execKillGrace):
	}
	killProcessGroup(cmd)
	if !exited {
		<-waited
	}
}

var errAgentOutputLimit = errors.New("agent output limit exceeded")

// boundedBuffer collects stdout up to a limit. The first write that would pass
// it is refused, which stops the copy from the pipe, and exceeded is closed so
// the turn can kill the process instead of waiting for it to finish flooding.
type boundedBuffer struct {
	mu       sync.Mutex
	limit    int
	buf      bytes.Buffer
	over     bool
	exceeded chan struct{}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over {
		return 0, errAgentOutputLimit
	}
	if b.buf.Len()+len(p) > b.limit {
		b.over = true
		close(b.exceeded)
		return 0, errAgentOutputLimit
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over
}

// Bytes returns what was collected. Call it after the process has been waited
// for, when nothing writes any more.
func (b *boundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.buf.Bytes())
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(p)
	if len(p) > b.max {
		p = p[len(p)-b.max:]
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.max:]...)
	}
	return written, nil
}

// String returns the kept tail as text. A cut that fell inside a multi-byte
// character is replaced rather than printed as invalid UTF-8.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(b.buf), "?"))
}
