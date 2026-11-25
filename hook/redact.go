/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package hook

// Redactor returns s with secrets (keys, tokens) replaced. The engine applies
// the agent's redactor to the error text and the content it logs; the errors
// it returns (TurnResult.Err) are left as they are, for the host to render.
type Redactor func(s string) string
