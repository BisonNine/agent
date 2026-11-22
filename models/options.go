package models

import (
	"log/slog"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/modelspec"
)

// component tags every record the package logs.
const component = "models"

// Option configures a Catalog built by New or Build.
type Option func(*settings)

// settings are a catalogue's options. They are fixed once New returns and
// travel with the catalogue's contents, so Adopt takes the new catalogue's.
type settings struct {
	cooldown      modelspec.Cooldown
	observer      Observer
	logger        *slog.Logger
	logContent    bool
	clock         clock.Clock
	baseEnv       func() []string
	modesFor      func(path string) fsmode.Modes
	declined      func(Declined) string
	limitDefaults modelspec.LimitDefaults
	store         SelectionStore
	fallbacks     map[Purpose]Purpose
	secrets       modelspec.Resolver
}

func defaultSettings() *settings {
	return &settings{
		observer:  nopObserver{},
		logger:    slog.New(slog.DiscardHandler),
		baseEnv:   DefaultBaseEnv,
		modesFor:  func(string) fsmode.Modes { return fsmode.Modes{} },
		declined:  DefaultDeclinedMessage,
		fallbacks: map[Purpose]Purpose{Worker: Chat},
	}
}

// WithCooldown sets how long a failing model is taken out of rotation, per
// HTTP-status category. The zero value is the built-in defaults.
func WithCooldown(c modelspec.Cooldown) Option {
	return func(s *settings) { s.cooldown = c }
}

// WithObserver sets the receiver of model health and fallback events. Its
// methods are called on the goroutine that made the call, with no lock held;
// they must not block.
func WithObserver(o Observer) Option {
	return func(s *settings) {
		if o != nil {
			s.observer = o
		}
	}
}

// WithLogger sets the logger of the catalogue and of the clients it builds.
// Without one nothing is logged.
func WithLogger(l *slog.Logger) Option {
	return func(s *settings) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithLogContent lets the CLI clients the catalogue builds log message content
// (a CLI's output when it fails or returns nothing). Off by default.
func WithLogContent(v bool) Option {
	return func(s *settings) { s.logContent = v }
}

// WithClock sets the clock cooldowns and same-model retries run on. Default:
// the system clock.
func WithClock(c clock.Clock) Option {
	return func(s *settings) { s.clock = c }
}

// WithBaseEnv sets the environment a CLI subprocess starts from, in "K=V"
// form. It is called each time a CLI client is built. A model's own env is
// overlaid on it. Default: DefaultBaseEnv, never the whole process
// environment.
func WithBaseEnv(fn func() []string) Option {
	return func(s *settings) {
		if fn != nil {
			s.baseEnv = fn
		}
	}
}

// WithFileModes sets the modes of a model's response log file and of the
// directories created for it. Default: fsmode's owner-only modes.
func WithFileModes(m fsmode.Modes) Option {
	return func(s *settings) { s.modesFor = func(string) fsmode.Modes { return m } }
}

// WithFileModesFor is WithFileModes for a host whose modes depend on where a
// file lies: fn is called with the response log file's path when a client is
// built.
func WithFileModesFor(fn func(path string) fsmode.Modes) Option {
	return func(s *settings) {
		if fn != nil {
			s.modesFor = fn
		}
	}
}

// WithDeclinedMessage sets the text of the error a CLI client returns when the
// CLI declined to use tools because its permission bypass is off. Default:
// DefaultDeclinedMessage.
func WithDeclinedMessage(fn func(Declined) string) Option {
	return func(s *settings) {
		if fn != nil {
			s.declined = fn
		}
	}
}

// WithLimitDefaults sets the limits a model that sets none of its own runs
// with (Chain.Limits, Candidate.Limits).
func WithLimitDefaults(d modelspec.LimitDefaults) Option {
	return func(s *settings) { s.limitDefaults = d }
}

// WithSelectionStore persists selections (Set.Select) so they survive a
// restart. Without one they are held in memory only.
func WithSelectionStore(st SelectionStore) Option {
	return func(s *settings) { s.store = st }
}

// WithFallback declares that a purpose with no models of its own uses those of
// another. Worker → Chat is declared by default; WithFallback(p, "") removes
// a rule.
func WithFallback(from, to Purpose) Option {
	return func(s *settings) {
		if to == "" {
			delete(s.fallbacks, from)
			return
		}
		s.fallbacks[from] = to
	}
}

// WithSecrets resolves the secret references of the document Build is given
// (modelspec.ResolveSecrets). Without it the document's values are used as
// they are. Ignored by New.
func WithSecrets(r modelspec.Resolver) Option {
	return func(s *settings) { s.secrets = r }
}
