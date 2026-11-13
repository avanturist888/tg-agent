package t3

// Proposed typed ingress contract. No production transport is connected to it.
// Native Codex requests are built for the T3 adapter, never sent by tg-agent.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const ServiceEventVersion = 1

var ErrServiceEventsUnsupported = errors.New("T3 service event ingress is not connected")

// ServiceEventIngress is implemented by a supported T3 endpoint, not by direct
// access to its shared app-server. No implementation is installed in tg-agent.
type ServiceEventIngress interface {
	EnqueueServiceEvent(context.Context, ServiceEvent) (ServiceEventReceipt, error)
}

func SubmitServiceEvent(ctx context.Context, ingress ServiceEventIngress, event ServiceEvent) (ServiceEventReceipt, error) {
	if err := event.Validate(); err != nil {
		return ServiceEventReceipt{}, err
	}
	if ingress == nil {
		return ServiceEventReceipt{}, ErrServiceEventsUnsupported
	}
	receipt, err := ingress.EnqueueServiceEvent(ctx, event)
	if err != nil {
		return ServiceEventReceipt{}, err
	}
	if err := event.ValidateReceipt(receipt); err != nil {
		return ServiceEventReceipt{}, err
	}
	return receipt, nil
}

type ServiceEvent struct {
	Version        int    `json:"version"`
	Source         string `json:"source"`
	SourceEventID  string `json:"sourceEventID"`
	ThreadID       string `json:"threadID"`
	NativeThreadID string `json:"nativeThreadID"`
	Text           string `json:"text"`
}

func (e ServiceEvent) Validate() error {
	if e.Version != ServiceEventVersion || e.Source != "tg-agent" {
		return errors.New("unsupported service event version or source")
	}
	for _, value := range []string{e.SourceEventID, e.ThreadID, e.NativeThreadID} {
		if len(value) == 0 || len(value) > 128 || !utf8.ValidString(value) {
			return errors.New("invalid service event identity")
		}
		for _, ch := range value {
			if ch <= ' ' || ch >= 127 {
				return errors.New("invalid service event identity")
			}
		}
	}
	if strings.TrimSpace(e.Text) == "" || len(e.Text) > 8*1024 || !utf8.ValidString(e.Text) {
		return errors.New("invalid service event payload")
	}
	output, err := e.toolPayload()
	if err != nil || len(output) > 16*1024 {
		return errors.New("encoded service event payload is too large")
	}
	return nil
}

// Fingerprint binds immutable content. NativeThreadID is a routing hint,
// deliberately excluded: provider handoff must not turn a retry into a new event.
func (e ServiceEvent) Fingerprint() (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	return e.fingerprint(), nil
}

func (e ServiceEvent) fingerprint() string {
	hash := sha256.New()
	hash.Write([]byte("tg-agent/service-event/v1\x00"))
	for _, field := range []string{e.Source, e.SourceEventID, e.ThreadID, e.Text} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		hash.Write(size[:])
		hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type ServiceEventReceipt struct {
	Source        string `json:"source"`
	SourceEventID string `json:"sourceEventID"`
	ThreadID      string `json:"threadID"`
	Fingerprint   string `json:"fingerprint"`
	Accepted      bool   `json:"accepted"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	Duplicate     bool   `json:"duplicate"`
	HistoryItemID string `json:"historyItemID"`
}

// ValidateReceipt checks durable T3 acceptance, not model processing. A timeout
// without this receipt is uncertain; retry the same event ID, never mint a new one.
func (e ServiceEvent) ValidateReceipt(r ServiceEventReceipt) error {
	hash, err := e.Fingerprint()
	if err != nil {
		return err
	}
	if !r.Accepted || r.Source != e.Source || r.SourceEventID != e.SourceEventID || r.ThreadID != e.ThreadID ||
		r.Fingerprint != hash || r.HistoryItemID == "" {
		return errors.New("service event receipt does not match")
	}
	switch r.State {
	case "pending", "provider_uncertain", "provider_accepted":
		if r.Reason == "" {
			return nil
		}
	case "not_delivered":
		switch r.Reason {
		case "target_archived", "target_deleted", "provider_unsupported":
			return nil
		}
	}
	return errors.New("invalid service event delivery outcome")
}

type ServiceToolOutput struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Output    string `json:"output"`
}

type ServiceTurnInput struct {
	ThreadID   string            `json:"threadId"`
	Input      []any             `json:"input"`
	ToolOutput ServiceToolOutput `json:"toolOutput"`
}

// CodexInput prepares bytes for T3's own provider adapter. T3 MUST resolve and
// verify the current native mapping under its lifecycle lock before calling it.
// The payload remains tool output; callers cannot select a developer/system role.
func (e ServiceEvent) CodexInput() (ServiceTurnInput, error) {
	if err := e.Validate(); err != nil {
		return ServiceTurnInput{}, err
	}
	output, err := e.toolPayload()
	if err != nil {
		return ServiceTurnInput{}, err
	}
	return ServiceTurnInput{
		ThreadID: e.NativeThreadID,
		Input:    []any{},
		ToolOutput: ServiceToolOutput{
			Name: "notification", Namespace: "tg-agent", Output: string(output),
		},
	}, nil
}

func (e ServiceEvent) toolPayload() ([]byte, error) {
	return json.Marshal(struct {
		Source        string `json:"source"`
		SourceEventID string `json:"sourceEventID"`
		ThreadID      string `json:"threadID"`
		Fingerprint   string `json:"fingerprint"`
		Trust         string `json:"trust"`
		Text          string `json:"text"`
	}{e.Source, e.SourceEventID, e.ThreadID, e.fingerprint(), "untrusted", e.Text})
}
