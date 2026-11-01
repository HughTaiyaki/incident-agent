// Package scripted supplies explicit developer decisions for offline harness verification.
// It is not a model and cannot measure diagnosis quality.
package scripted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
)

// MaxScriptBytes bounds an imported offline decision script.
const MaxScriptBytes = 256 << 10

// Script is consumed sequentially by one run.
type Script struct {
	Request   investigation.Request `json:"request"`
	Decisions []json.RawMessage     `json:"decisions"`
	next      int
}

// Read enforces a finite script; individual decisions are validated and charged by the harness.
func Read(reader io.Reader) (*Script, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxScriptBytes+1))
	if err != nil || len(data) > MaxScriptBytes {
		return nil, errors.New("cannot read bounded investigation script")
	}
	// Decode has a per-decision limit; the envelope has its own larger bound.
	var s Script
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return nil, errors.New("invalid investigation script")
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("trailing investigation script data")
	}
	if len(s.Decisions) == 0 || len(s.Decisions) > 12 {
		return nil, errors.New("expected one to twelve scripted decisions")
	}
	if _, err = investigation.ValidateRequest(s.Request); err != nil {
		return nil, err
	}
	return &s, nil
}

// Decide returns the next scripted JSON value, detached from the source buffer.
func (s *Script) Decide(ctx context.Context, _ investigation.View) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.next >= len(s.Decisions) {
		return nil, errors.New("script exhausted")
	}
	raw := append(json.RawMessage{}, s.Decisions[s.next]...)
	s.next++
	return raw, nil
}
