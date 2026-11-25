/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"log/slog"
	"sync/atomic"
)

// component is the component attribute of the records the Worker and the
// CLI providers log.
const component = "provider"

// discard returns a logger that drops every record.
func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// logging is what a CLI provider logs to and whether it may log message
// content. The zero value logs nothing and no content. Both are atomic so a
// host may change them while a call is running.
type logging struct {
	logger     atomic.Pointer[slog.Logger]
	logContent atomic.Bool
}

// SetLogger sets the logger the provider writes to. Nil, the default,
// discards every record.
func (l *logging) SetLogger(logger *slog.Logger) { l.logger.Store(logger) }

// SetLogContent sets whether the provider may log message content: the CLI's
// stdout and stderr when it fails or returns nothing. Off by default.
func (l *logging) SetLogContent(v bool) { l.logContent.Store(v) }

// log returns the provider's logger tagged with its component.
func (l *logging) log() *slog.Logger {
	logger := l.logger.Load()
	if logger == nil {
		return discard()
	}
	return logger.With("component", component)
}
