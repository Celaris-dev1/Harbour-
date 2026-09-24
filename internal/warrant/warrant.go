// Package warrant is a small client for Warrant's authorize endpoint
// (see https://github.com/.../Warrant, internal/broker/http.go and
// internal/broker/service.go for the canonical shapes this mirrors).
//
// When WARRANT_URL is set, every Harbour effect must be authorized: the
// executor POSTs {token, svid, call:{tool,resource,args}} to
// {WARRANT_URL}/v1/authorize before invoking a tool, using the token/svid
// supplied with the goal at submit time. A deny (or an unreachable Warrant)
// fails that attempt without ever calling the tool; the worker then pauses
// the goal with the reason, and an operator resolves it (revoke/re-mint the
// token, or override) before resume, which re-authorizes from scratch.
package warrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Call mirrors Warrant's internal/token.Call: the tool, an optional
// resource, and the call's arguments.
type Call struct {
	Tool     string         `json:"tool"`
	Resource string         `json:"resource,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
}

// authorizeRequest mirrors Warrant's broker.AuthorizeRequest.
type authorizeRequest struct {
	Token    string `json:"token"`
	SVID     string `json:"svid"`
	Approval string `json:"approval,omitempty"`
	Call     Call   `json:"call"`
}

// Decision mirrors Warrant's broker.Decision (the PEP verdict).
type Decision struct {
	Allow      bool   `json:"allow"`
	Reason     string `json:"reason"`
	TokenID    string `json:"token_id,omitempty"`
	ActionHash string `json:"action_hash,omitempty"`
}

// Client is what the executor needs: one call, one verdict.
type Client interface {
	Authorize(ctx context.Context, token, svid string, call Call) (Decision, error)
}

// HTTP talks to a real Warrant broker's POST /v1/authorize.
type HTTP struct {
	BaseURL string
	Client  *http.Client
}

// FromEnv returns an HTTP client if WARRANT_URL is set, else nil (meaning:
// no Warrant integration, effects run unauthorized — the executor treats a
// nil Client as "Warrant is off", not as "deny"). Once WARRANT_URL is set,
// authorization is mandatory for every effect: there is no per-goal opt-out.
func FromEnv() Client {
	u := os.Getenv("WARRANT_URL")
	if u == "" {
		return nil
	}
	return &HTTP{BaseURL: u, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *HTTP) Authorize(ctx context.Context, token, svid string, call Call) (Decision, error) {
	b, err := json.Marshal(authorizeRequest{Token: token, SVID: svid, Call: call})
	if err != nil {
		return Decision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/v1/authorize", bytes.NewReader(b))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("warrant: %w", err)
	}
	defer resp.Body.Close()
	var d Decision
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return Decision{}, fmt.Errorf("warrant: decode response: %w", err)
	}
	if resp.StatusCode/100 != 2 && d.Reason == "" {
		d.Reason = fmt.Sprintf("warrant: http status %d", resp.StatusCode)
	}
	return d, nil
}
