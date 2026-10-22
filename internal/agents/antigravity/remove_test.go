package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

func TestAntigravityRemoveGlobalCleansFootprintAndPreservesUnrelatedContent(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true
	env.InstallGlobalInstructions = true
	a := New()

	// 1. Seed unrelated MCP servers and unrelated hook plugins to ensure they survive.
	mcpPath := filepath.Join(env.Home, ".gemini", "config", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(mcpPath), 0o755); err != nil {
		t.Fatal(err)
	}
	initialMCP := map[string]any{
		"mcpServers": map[string]any{
			"other-server": map[string]any{"command": "other-bin"},
		},
	}
	mcpBytes, _ := json.Marshal(initialMCP)
	if err := os.WriteFile(mcpPath, mcpBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	hookPath := HookConfigPath(env.Home)
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	initialHooks := map[string]any{
		"user-plugin": map[string]any{
			"SessionStart": []any{
				map[string]any{"type": "command", "command": "echo user"},
			},
		},
	}
	hookBytes, _ := json.Marshal(initialHooks)
	if err := os.WriteFile(hookPath, hookBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Apply global configuration.
	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	artifacts := GlobalArtifacts(env.Home)
	if len(artifacts) == 0 {
		t.Fatal("expected non-empty GlobalArtifacts after Apply")
	}

	for _, wantSubstring := range []string{"mcp_config.json", "hooks.json", "plugin.json", "AGENTS.md", "metadata.json", "gortex-instructions.md"} {
		found := false
		for _, art := range artifacts {
			if slices.Contains(stringsSplit(art, string(filepath.Separator)), wantSubstring) || filepath.Base(art) == wantSubstring {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected GlobalArtifacts to contain artifact for %q, got: %v", wantSubstring, artifacts)
		}
	}

	// 3. RemoveGlobal.
	removed, failures := a.RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) > 0 {
		t.Fatalf("RemoveGlobal reported failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("expected RemoveGlobal to remove artifacts")
	}

	// 4. Verify Gortex footprint is cleaned up.
	remaining := GlobalArtifacts(env.Home)
	if len(remaining) > 0 {
		t.Errorf("expected empty GlobalArtifacts after RemoveGlobal, got: %v", remaining)
	}

	// 5. Verify unrelated MCP server survived.
	mcpAfterData, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read mcp_config after remove: %v", err)
	}
	var mcpAfter map[string]any
	if err := json.Unmarshal(mcpAfterData, &mcpAfter); err != nil {
		t.Fatal(err)
	}
	servers, ok := mcpAfter["mcpServers"].(map[string]any)
	if !ok || servers["other-server"] == nil {
		t.Errorf("unrelated MCP server was not preserved: %v", mcpAfter)
	}
	if servers["gortex"] != nil {
		t.Errorf("gortex MCP server was not removed: %v", mcpAfter)
	}

	// 6. Verify unrelated hook plugin survived.
	hookAfterData, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hooks.json after remove: %v", err)
	}
	var hookAfter map[string]any
	if err := json.Unmarshal(hookAfterData, &hookAfter); err != nil {
		t.Fatal(err)
	}
	if hookAfter["user-plugin"] == nil {
		t.Errorf("unrelated hook plugin was not preserved: %v", hookAfter)
	}
	if hookAfter["gortex"] != nil {
		t.Errorf("gortex hook plugin was not removed: %v", hookAfter)
	}
}

func TestAntigravityRemoveGlobalDeletesHookFileWhenOnlyGortex(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true
	env.InstallGlobalInstructions = true
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	hookPath := HookConfigPath(env.Home)
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("hooks.json missing before remove: %v", err)
	}

	// Also add user prose around the rules block in AGENTS.md.
	insPath := GlobalInstructionsPath(env.Home)
	existingRules, _ := os.ReadFile(insPath)
	customRules := "# My User Policy\n\n" + string(existingRules) + "\n\n# User Footer\n"
	if err := os.WriteFile(insPath, []byte(customRules), 0o644); err != nil {
		t.Fatal(err)
	}

	// Also customize the Knowledge Item metadata.
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metaPath := filepath.Join(kiBase, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(`{"custom":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, failures := a.RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) > 0 {
		t.Fatalf("failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("expected removals")
	}

	// 1. hooks.json was purely Gortex, so it should be deleted.
	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Errorf("expected hooks.json to be deleted, err=%v", err)
	}

	// 2. AGENTS.md had user prose, so the file should remain with the block stripped.
	data, err := os.ReadFile(insPath)
	if err != nil {
		t.Fatalf("expected AGENTS.md to survive with user prose, err=%v", err)
	}
	text := string(data)
	if !slices.Contains(stringsSplit(text, "\n"), "# My User Policy") || !slices.Contains(stringsSplit(text, "\n"), "# User Footer") {
		t.Errorf("user prose lost:\n%s", text)
	}
	if slices.Contains(stringsSplit(text, "\n"), agents.GlobalRulesStartMarker) {
		t.Errorf("Gortex rule block not stripped:\n%s", text)
	}

	// 3. Customized Knowledge Item metadata should be preserved.
	metaData, err := os.ReadFile(metaPath)
	if err != nil || string(metaData) != `{"custom":true}` {
		t.Errorf("customized metadata.json was not preserved: %s (err=%v)", string(metaData), err)
	}
}

func TestAntigravityRemoveHelperCorruptOrMissingFiles(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// 1. Non-existent files return empty artifacts and no failures.
	if arts := GlobalArtifacts(env.Home); len(arts) != 0 {
		t.Errorf("expected 0 artifacts on empty home, got %v", arts)
	}

	// 2. Corrupt JSON in plugin.json is not considered owned.
	corruptManifest := filepath.Join(env.Home, "plugin.json")
	if err := os.WriteFile(corruptManifest, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isOwnedPluginManifest(corruptManifest) {
		t.Error("corrupt plugin.json should not be owned")
	}

	// 3. plugin.json with other name is not considered owned.
	otherManifest := filepath.Join(env.Home, "other.json")
	if err := os.WriteFile(otherManifest, []byte(`{"name":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if isOwnedPluginManifest(otherManifest) {
		t.Error("other plugin.json should not be owned")
	}
}

func stringsSplit(s, sep string) []string {
	var parts []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			parts = append(parts, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
