/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package session_test

import (
	"testing"

	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/ctxengine/session/sessiontest"
)

func TestSQLiteStoreConformance(t *testing.T) {
	sessiontest.Run(t, func(t *testing.T) sessiontest.Store {
		t.Helper()
		s, err := session.NewSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return s
	})
}

func TestMemoryStoreConformance(t *testing.T) {
	sessiontest.Run(t, func(t *testing.T) sessiontest.Store {
		t.Helper()
		return session.NewMemory()
	})
}
