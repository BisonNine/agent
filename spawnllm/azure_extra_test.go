/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"testing"

	"go.bison9.com/agent/spawnllm/azure"
)

// Tests for azure sub-package functions not covered.

func TestAzureProvider_GetDefaultModel(t *testing.T) {
	p := azure.NewProvider("test-key", "https://example.openai.azure.com", "")
	model := p.GetDefaultModel()
	// Azure deployments are user-configured; GetDefaultModel returns empty string.
	if model != "" {
		t.Errorf("GetDefaultModel() = %q, want empty (Azure uses deployment names)", model)
	}
}
