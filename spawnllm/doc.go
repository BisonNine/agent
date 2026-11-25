/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package spawnllm holds the provider clients (CLI and HTTP) and the types
// they share: messages, responses, tool definitions, error classification.
// Each client makes one model call; the tool loop is the agent loop's
// (go.bison9.com/agent/loop), fallback and cooldown are package models'.
//
// spawnllm is deliberately dependency-free of any host: it imports only
// go.bison9.com/agent/toolspec, accounting, fsmode and the standard library
// (plus provider SDKs).
package spawnllm
