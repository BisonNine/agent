/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package mcphost

import (
	"context"
	"sync"

	"go.bison9.com/agent/hook"
)

// Served is the agent's hook.ServedTools over a token store and one CLI
// endpoint: for each CLI-model turn it issues a token for the turn's scope,
// returns the endpoint's prompt text for it (CLIConfig.Access; for an
// Argument endpoint, the token prompt), and revokes the token when the turn
// ends or is stopped.
type Served struct {
	Store *Store
	CLI   CLIConfig
}

// ServeTurn issues the turn's token and returns its prompt text and revoke.
func (s Served) ServeTurn(_ context.Context, scope hook.Scope) (string, func(), error) {
	token, err := s.Store.Issue(Grant{Scope: scope})
	if err != nil {
		return "", nil, err
	}
	var once sync.Once
	end := func() { once.Do(func() { s.Store.Revoke(token) }) }
	return s.CLI.Access(token).Prompt, end, nil
}
