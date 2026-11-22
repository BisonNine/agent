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
// License: MIT
// Copyright (c) 2026 Tenebris Technologies Inc.
package toolkit
