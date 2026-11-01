// Package runstate defines the application persistence port.
package runstate

import "context"

// Command is trusted application input, never a model-facing tool schema.
// JSON strings inside Request/Input/Result/Report must pass application guards.
type Command struct {
	Checkpoint string `json:"checkpoint"`
	Action     string `json:"action"`
	ID         string `json:"id"`
	Request    string `json:"request"`
	Deadline   int64  `json:"deadline"`
	MaxActive  int    `json:"max_active"`
	Owner      string `json:"owner"`
	Epoch      int64  `json:"epoch"`
	Version    int64  `json:"version"`
	Operation  string `json:"operation"`
	Kind       string `json:"kind"`
	Cost       int    `json:"cost"`
	Input      string `json:"input"`
	Result     string `json:"result"`
	Report     string `json:"report"`
}

// State is the persistence snapshot, distinct from the investigation Run schema.
type State struct {
	Checkpoint     string               `json:"checkpoint"`
	Schema         int                  `json:"schema"`
	ID             string               `json:"id"`
	Request        string               `json:"request"`
	Created        int64                `json:"created"`
	Deadline       int64                `json:"deadline"`
	Status         string               `json:"status"`
	Owner          string               `json:"owner"`
	Epoch          int64                `json:"epoch"`
	Version        int64                `json:"version"`
	LeaseUntil     int64                `json:"lease_until"`
	Cancelled      bool                 `json:"cancelled"`
	Tokens         int                  `json:"tokens"`
	Requests       int                  `json:"requests"`
	ModelCalls     int                  `json:"model_calls"`
	Operations     map[string]Operation `json:"operations"`
	OperationCount int                  `json:"operation_count"`
	ResultBytes    int                  `json:"result_bytes"`
	Report         string               `json:"report"`
	Finished       int64                `json:"finished"`
}

// Operation retains frozen parameters, reservations and committed results.
type Operation struct {
	Kind     string `json:"kind"`
	Cost     int    `json:"cost"`
	Input    string `json:"input"`
	Status   string `json:"status"`
	Epoch    int64  `json:"epoch"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
	Result   string `json:"result"`
}

// Reply preserves protocol codes. In particular ALREADY is not permission to
// repeat an external call; only a newly successful Begin permits its first call.
type Reply struct {
	Code  string `json:"code"`
	State *State `json:"state"`
	// Now is Redis time in Unix milliseconds, so readers need no local clock.
	Now int64 `json:"now"`
}

// Store atomically executes commands and discovers bounded ready/expired candidates.
type Store interface {
	Execute(context.Context, Command) (Reply, error)
	Candidates(context.Context, int) ([]string, error)
}
