// Package ledger is a small HTTP client for the Ledger records API. When
// LEDGER_URL is unset it is a no-op recorder.
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type Actor struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Model        string `json:"model,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`
}

type Record struct {
	Chain         string         `json:"chain"`
	Type          string         `json:"type"`
	GoalID        string         `json:"goal_id,omitempty"`
	ActorChain    []Actor        `json:"actor_chain"`
	PolicyVersion string         `json:"policy_version,omitempty"`
	Payload       map[string]any `json:"payload"`
}

type Recorder interface {
	Record(ctx context.Context, r Record) error
}

type Noop struct{}

func (Noop) Record(context.Context, Record) error { return nil }

type HTTP struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// FromEnv returns an HTTP recorder if LEDGER_URL is set, else Noop.
func FromEnv() Recorder {
	u := os.Getenv("LEDGER_URL")
	if u == "" {
		return Noop{}
	}
	return &HTTP{BaseURL: u, Token: os.Getenv("LEDGER_TOKEN"), Client: &http.Client{Timeout: 5 * time.Second}}
}

func (h *HTTP) Record(ctx context.Context, r Record) error {
	r.Chain = "harbour"
	if len(r.ActorChain) == 0 || r.ActorChain[0].Kind != "human" {
		return fmt.Errorf("ledger: actor_chain must start with a human")
	}
	b, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/v1/records", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ledger: status %d", resp.StatusCode)
	}
	return nil
}
