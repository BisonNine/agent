/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package modelspec is the one definition of a provider and a model that
// hosts and the rest of the SDK share: the Provider and Model types with their
// JSON form, protocol rules and validation, the table of supported CLI agents,
// the cooldown policy, a versioned Document that holds a whole model
// configuration, secret references, and the per-request Limits computed from
// a model.
//
// It is plain data. Nothing here opens a connection, reads the environment or
// reads a file unless the caller asks for it (LoadFile, DefaultResolver), and
// nothing logs.
package modelspec
