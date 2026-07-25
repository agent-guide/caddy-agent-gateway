// Package runtimeapi defines the runtime-neutral execution boundary for
// operator-facing Agents.
//
// The package may depend on pkg/agent for the stable Agent definition. Lower
// protocol packages such as pkg/acp, pkg/llm, and pkg/mcp must not depend on
// this package or on pkg/agent. Gateway-owned adapters sit above both sides and
// translate native runtime requests and events into these contracts.
//
// Runtimeapi owns contracts, not runtime lifecycle or transport behavior.
// Backends remain responsible for their native processes, sessions,
// checkpoints, and protocol details.
package runtimeapi
