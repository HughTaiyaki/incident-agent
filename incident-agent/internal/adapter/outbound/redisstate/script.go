// Package redisstate contains the versioned atomic Redis persistence protocol.
// It is not yet connected to the investigation runner.
package redisstate

import _ "embed"

// Script accepts three KEYS (state, ready, leases) and one JSON command ARGV.
// Its JSON reply has code and, on success, state. See the persistence guide.
//
//go:embed state.lua
var Script string
