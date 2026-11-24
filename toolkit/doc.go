// Package toolkit holds the tool core: Tool and its optional interfaces
// (AsyncExecutor, SessionScoped, ExternalNamer, DiscoveryGrouped), Result,
// Refusal, the toolspec bridge (FromSpec), the schema helpers, the per-call
// context values (WithChat, WithSessionKey, WithAgentID) and Tally.
//
// The live registry: a Catalog holds every tool the host mounted. It is
// safe to change from any goroutine at any time; each model call takes a
// Snapshot, so a change takes effect at the next call, and Subscribe
// reports every change.
//
// Two levels, the same shape as the models package: the Catalog is
// process-wide, and a Set is the tools one agent is granted, each listed
// or hidden. The catalogue's read methods are its default set's, so a host
// with one agent never sees the two levels. Both satisfy Registry, what an
// agent is given. A configuration reload builds a new catalogue and hands
// it over with Adopt. Tools can be switched off and on at either level
// (Disable, Enable) without a reload. See README.md.
//
// License: MIT
// Copyright (c) 2026 Tenebris Technologies Inc.
package toolkit
