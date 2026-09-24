// Package registry holds pluggable tools and agents.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Tool performs one side effect. The idempotency key is passed so tools
// talking to external systems can forward it (e.g. as an Idempotency-Key header).
type Tool interface {
	Name() string
	Execute(ctx context.Context, key string, args json.RawMessage) (json.RawMessage, error)
}

// ProbeResult is what a reconciliation probe learned about an effect.
type ProbeResult struct {
	Happened bool
	Result   json.RawMessage
}

// Prober is optionally implemented by tools that can check whether an effect
// with a given idempotency key already happened externally.
type Prober interface {
	Probe(ctx context.Context, key string, args json.RawMessage) (ProbeResult, error)
}

// Provenance sources for inbound messages.
const (
	SrcOperator          = "operator"
	SrcPeerAgent         = "peer_agent"
	SrcScheduler         = "scheduler"
	SrcToolResult        = "tool_result"
	SrcRetrievedDocument = "retrieved_document"
)

var Sources = []string{SrcOperator, SrcPeerAgent, SrcScheduler, SrcToolResult, SrcRetrievedDocument}

func ValidSource(s string) bool {
	for _, x := range Sources {
		if x == s {
			return true
		}
	}
	return false
}

// Message is an inbound message with provenance, visible to agents.
type Message struct {
	ID        int64           `json:"id"`
	Channel   string          `json:"channel"`
	Source    string          `json:"source"`
	Sender    string          `json:"sender"`
	Trusted   bool            `json:"trusted"`
	Body      json.RawMessage `json:"body"`
	CreatedAt time.Time       `json:"created_at"`
}

// StepResult is a committed step.
type StepResult struct {
	Step   int             `json:"step"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Result json.RawMessage `json:"result"`
}

// StepContext is what an agent sees when deciding its next action.
type StepContext struct {
	GoalID   string
	Step     int
	Input    json.RawMessage
	History  []StepResult
	Messages []Message
}

// Action is an agent decision: call a tool, or finish.
type Action struct {
	Tool   string          `json:"tool,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Finish bool            `json:"finish,omitempty"`
	Fail   string          `json:"fail,omitempty"`
	// RequiresApproval flags a tool call whose inputs were derived from
	// untrusted-sourced content (peer_agent, retrieved_document, or any
	// other message with Trusted=false). Only the agent can know this: the
	// provenance tag says where a message came from, but only the agent
	// knows whether it treated that content as an instruction. When set,
	// the worker never invokes the tool directly; it records the call as
	// needs_review and pauses the goal, exactly like an in-doubt effect
	// with no reconciliation probe, so an operator must approve
	// (`harbour resolve <id> committed|retry`) before it can run.
	RequiresApproval bool   `json:"requires_approval,omitempty"`
	ApprovalReason   string `json:"approval_reason,omitempty"`
}

// UntrustedInputPresent reports whether sc.Messages contains any
// message that is not Trusted, i.e. not from the operator. Agents can use
// this as a cheap default policy: any tool call planned while untrusted
// content is present in the context should set Action.RequiresApproval,
// unless the agent has its own narrower logic for which untrusted content
// it actually used to decide.
func UntrustedInputPresent(msgs []Message) bool {
	for _, m := range msgs {
		if !m.Trusted {
			return true
		}
	}
	return false
}

// Agent decides next actions. Next must be a deterministic function of the
// StepContext for resume to be exact; Harbour additionally never re-asks the
// agent for a step whose intent is already committed.
type Agent interface {
	Name() string
	Next(ctx context.Context, sc StepContext) (Action, error)
	// Verify runs in the verifying state; nil means done.
	Verify(ctx context.Context, sc StepContext) error
}

type Registry struct {
	mu     sync.RWMutex
	tools  map[string]Tool
	agents map[string]Agent
}

func New() *Registry { return &Registry{tools: map[string]Tool{}, agents: map[string]Agent{}} }

func (r *Registry) AddTool(t Tool)   { r.mu.Lock(); r.tools[t.Name()] = t; r.mu.Unlock() }
func (r *Registry) AddAgent(a Agent) { r.mu.Lock(); r.agents[a.Name()] = a; r.mu.Unlock() }

func (r *Registry) Tool(n string) (Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[n]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", n)
	}
	return t, nil
}

func (r *Registry) Agent(n string) (Agent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[n]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", n)
	}
	return a, nil
}

func (r *Registry) AgentNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for n := range r.agents {
		out = append(out, n)
	}
	return out
}
