package antigravity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestAntigravityRegistersMCPAndWritesKI is the acceptance test for
// the Antigravity adapter: it must write both native MCP configs,
// native lifecycle hooks (~/.gemini/config/hooks.json), the Knowledge Item,
// and in project mode the AGENTS.md community routing block and generated skills.
func TestAntigravityRegistersMCPAndWritesKI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// Seed ~/.gemini/config so Detect succeeds hermetically.
	if err := os.MkdirAll(filepath.Join(env.Home, ".gemini", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	plan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Files) == 0 {
		t.Fatal("expected files in plan")
	}

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	// 1. Native MCP stanzas under ~/.gemini/antigravity/mcp_config.json (IDE)
	//    and ~/.gemini/config/mcp_config.json (agy CLI / Antigravity 2.0).
	for _, mcpPath := range mcpConfigPaths(env.Home) {
		if _, err := os.Stat(mcpPath); err != nil {
			t.Fatalf("mcp_config.json missing at %s: %v", mcpPath, err)
		}
		cfg := agentstest.ReadJSON(t, mcpPath)
		servers, ok := cfg["mcpServers"].(map[string]any)
		if !ok {
			t.Fatalf("mcpServers missing in %s: %v", mcpPath, cfg)
		}
		if _, ok := servers["gortex"]; !ok {
			t.Fatalf("gortex server missing from mcpServers in %s: %v", mcpPath, servers)
		}
	}

	// 1b. In project mode (gortex init), user-level lifecycle hooks are NOT installed.
	hooksPath := HookConfigPath(env.Home)
	if _, err := os.Stat(hooksPath); err == nil {
		t.Errorf("hooks.json should not be installed in ModeProject: %s", hooksPath)
	}

	// 2. Knowledge Item artifacts.
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	for _, p := range []string{
		filepath.Join(kiBase, "metadata.json"),
		filepath.Join(kiBase, "artifacts", "gortex-instructions.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("KI artifact missing: %s (%v)", p, err)
		}
	}
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatalf("read instructions: %v", err)
	}
	text := string(instructions)
	for _, want := range []string{
		agents.InstructionsSentinel,
		"Call `explore` first",
		"change(operation:\"impact\")",
		"Gortex MCP integration failure",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Antigravity instructions missing %q", want)
		}
	}
	for _, forbidden := range []string{"./gortex query", "facade-v1", "tools_search", "gortex call", "daemon start"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Antigravity instructions contain obsolete agent vocabulary %q", forbidden)
		}
	}

	// 3. Project mode routing + generated skills.
	if _, err := os.Stat(filepath.Join(env.Root, "AGENTS.md")); err != nil {
		t.Errorf("expected AGENTS.md in project root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".agents", "skills", "gortex-stub", "SKILL.md")); err != nil {
		t.Errorf("expected generated skill in .agents/skills/gortex-stub/SKILL.md: %v", err)
	}

	agentstest.AssertIdempotent(t, a, env)
}

func TestAntigravityGlobalModeInstallsRulesSkillsAndInspectsCleanly(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true
	env.InstallGlobalInstructions = true
	// Seed ~/.gemini/config so Detect succeeds hermetically.
	if err := os.MkdirAll(filepath.Join(env.Home, ".gemini", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	globalPlan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("global plan: %v", err)
	}
	if len(globalPlan.Files) == 0 {
		t.Fatal("expected files in global plan")
	}

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply global: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	if _, err := os.Stat(GlobalPluginManifestPath(env.Home)); err != nil {
		t.Fatalf("plugin.json missing: %v", err)
	}
	rulesBody, err := os.ReadFile(GlobalInstructionsPath(env.Home))
	if err != nil {
		t.Fatalf("global rules AGENTS.md missing: %v", err)
	}
	if !strings.Contains(string(rulesBody), agents.GlobalRulesStartMarker) {
		t.Errorf("global rules AGENTS.md missing start marker:\n%s", rulesBody)
	}

	for _, id := range CuratedSkillNames() {
		p := curatedSkillPath(env.Home, id)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing curated skill %s at %s: %v", id, p, err)
		}
	}

	// Verify Inspect sees everything Apply wrote.
	state := Inspect(env.Home)
	if !state.ConfigPresent || !state.MCPServer || !state.InstructionsWired || !state.PostToolUseDenyPosture {
		t.Fatalf("unexpected Inspect state: %+v", state)
	}
	for _, ev := range HookEvents {
		if state.Hooks[ev] != 1 {
			t.Errorf("Inspect hook count for %s = %d, want 1", ev, state.Hooks[ev])
		}
	}

	// Verify SyncSkills narrows to a subset.
	var buf bytes.Buffer
	subset := []string{"gortex-explore", "gortex-cli"}
	if _, err := SyncSkills(&buf, env.Home, subset, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	if _, err := os.Stat(curatedSkillPath(env.Home, "gortex-explore")); err != nil {
		t.Errorf("expected gortex-explore to remain after SyncSkills: %v", err)
	}
	if _, err := os.Stat(curatedSkillPath(env.Home, "gortex-debug")); !os.IsNotExist(err) {
		t.Errorf("expected gortex-debug to be pruned by SyncSkills, err=%v", err)
	}
}

func TestAntigravityMigratesExactV060KI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metadataPath := filepath.Join(kiBase, "metadata.json")
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	for path, content := range map[string]string{
		metadataPath:     v060Metadata,
		instructionsPath: v060Instructions,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for path, want := range map[string]string{
		metadataPath:     Metadata,
		instructionsPath: Instructions,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("v0.60.0 artifact %s was not migrated", path)
		}
	}
}

func TestAntigravityPreservesCustomizedKI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metadataPath := filepath.Join(kiBase, "metadata.json")
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	custom := map[string]string{
		metadataPath:     `{"summary":"my policy"}`,
		instructionsPath: "# Keep my custom workflow\n",
	}
	for path, content := range custom {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := New().Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for path, want := range custom {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("customized artifact %s was overwritten", path)
		}
	}
}

func TestAntigravityDetect(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	origLook := lookAntigravityBinary
	defer func() { lookAntigravityBinary = origLook }()
	lookAntigravityBinary = func() (string, error) { return "", os.ErrNotExist }

	// 1. Clean scratch environment -> false
	detected, err := a.Detect(env)
	if err != nil {
		t.Fatal(err)
	}
	if detected {
		t.Error("expected Detect=false on clean scratch environment")
	}

	// 2. Binary on PATH -> true
	lookAntigravityBinary = func() (string, error) { return "/bin/agy", nil }
	detected, err = a.Detect(env)
	if err != nil || !detected {
		t.Errorf("expected Detect=true with binary on PATH, got %v (err=%v)", detected, err)
	}
	lookAntigravityBinary = func() (string, error) { return "", os.ErrNotExist }

	// 3. ~/.gemini/config present -> true
	geminiConfig := filepath.Join(env.Home, ".gemini", "config")
	if err := os.MkdirAll(geminiConfig, 0o755); err != nil {
		t.Fatal(err)
	}
	detected, err = a.Detect(env)
	if err != nil || !detected {
		t.Errorf("expected Detect=true with ~/.gemini/config present, got %v (err=%v)", detected, err)
	}
}

func TestAntigravityHookModes(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		wantSub string
	}{
		{"consult-unlock", "--mode=consult-unlock"},
		{"nudge", "--mode=nudge"},
		{"enrich", "--mode=enrich"},
		{"deny", "hook --agent=antigravity --event="},
		{"", "hook --agent=antigravity --event="},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			env, _ := agentstest.NewEnv(t)
			env.Mode = agents.ModeGlobal
			env.InstallHooks = true
			env.HookMode = tc.mode
			a := New()

			if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			hooksPath := HookConfigPath(env.Home)
			data, err := os.ReadFile(hooksPath)
			if err != nil {
				t.Fatalf("read hooks: %v", err)
			}
			text := string(data)
			if !strings.Contains(text, tc.wantSub) {
				t.Errorf("hooks.json missing %q for mode %q:\n%s", tc.wantSub, tc.mode, text)
			}
			if (tc.mode == "deny" || tc.mode == "") && strings.Contains(text, "--mode=") {
				t.Errorf("deny/empty mode should not carry --mode flag:\n%s", text)
			}
		})
	}
}

func TestAntigravityCommunitiesRoutingPathFallsBackToGeminiMD(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	env := agents.Env{Root: root, Home: home, Mode: agents.ModeProject, SkillsRouting: "- test routing"}
	a := New()

	// 1. Without GEMINI.md, defaults to AGENTS.md.
	if p := a.CommunitiesRoutingPath(env); p != filepath.Join(root, "AGENTS.md") {
		t.Errorf("expected AGENTS.md, got %s", p)
	}

	// 2. With only GEMINI.md, returns GEMINI.md.
	geminiMD := filepath.Join(root, "GEMINI.md")
	if err := os.WriteFile(geminiMD, []byte("# Gemini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := a.CommunitiesRoutingPath(env); p != geminiMD {
		t.Errorf("expected GEMINI.md, got %s", p)
	}

	// 3. Plan plans merge for GEMINI.md and not AGENTS.md.
	plan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	hasGeminiAction := false
	for _, f := range plan.Files {
		if f.Path == geminiMD {
			hasGeminiAction = true
		}
		if f.Path == filepath.Join(root, "AGENTS.md") {
			t.Errorf("plan should not include AGENTS.md when GEMINI.md is present: %+v", plan.Files)
		}
	}
	if !hasGeminiAction {
		t.Errorf("plan missing GEMINI.md action: %+v", plan.Files)
	}

	// 4. Apply modifies GEMINI.md and does not create AGENTS.md.
	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, err := os.ReadFile(geminiMD)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- test routing") {
		t.Errorf("GEMINI.md missing routing block:\n%s", string(data))
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err == nil {
		t.Errorf("AGENTS.md was created when GEMINI.md was present")
	}

	// 5. GEMINI.md is only the fallback: once AGENTS.md exists it wins.
	agentsMD := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(agentsMD, []byte("# Agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := a.CommunitiesRoutingPath(env); p != agentsMD {
		t.Errorf("expected AGENTS.md when both files exist, got %s", p)
	}
}

func TestAntigravityAdapterMetadataAndPlanEmptyHome(t *testing.T) {
	a := New()
	if a.Name() != "antigravity" {
		t.Errorf("Name() = %q, want antigravity", a.Name())
	}
	if !strings.Contains(a.DocsURL(), "antigravity") {
		t.Errorf("DocsURL() = %q", a.DocsURL())
	}
	if !a.WritesSkillFiles() {
		t.Error("WritesSkillFiles() should be true")
	}
	if !a.WritesCommunitiesRouting() {
		t.Error("WritesCommunitiesRouting() should be true")
	}

	plan, err := a.Plan(agents.Env{Home: ""})
	if err != nil || len(plan.Files) != 0 {
		t.Errorf("Plan with empty Home = (%+v, %v), want empty plan", plan, err)
	}
}

func TestAntigravityCustomEnvOverrides(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil || realHome == "" {
		t.Skip("no user home dir")
	}

	custom := t.TempDir()
	t.Setenv("ANTIGRAVITY_CLI_HOME", custom)

	paths := mcpConfigPaths(realHome)
	wantCustomMCP := filepath.Join(custom, "mcp_config.json")
	found := false
	for _, p := range paths {
		if p == wantCustomMCP {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("mcpConfigPaths(%q) missing %q: %v", realHome, wantCustomMCP, paths)
	}

	if hookPath := HookConfigPath(realHome); hookPath != filepath.Join(custom, "hooks.json") {
		t.Errorf("HookConfigPath(%q) = %q, want %q", realHome, hookPath, filepath.Join(custom, "hooks.json"))
	}

	// AGY_HOME fallback
	t.Setenv("ANTIGRAVITY_CLI_HOME", "")
	t.Setenv("AGY_HOME", custom)
	if hookPath := HookConfigPath(realHome); hookPath != filepath.Join(custom, "hooks.json") {
		t.Errorf("HookConfigPath with AGY_HOME = %q, want %q", hookPath, filepath.Join(custom, "hooks.json"))
	}
}

func TestAntigravityHooksDisabledInspection(t *testing.T) {
	rootDisabled := map[string]any{
		"gortex": map[string]any{
			"enabled": false,
		},
	}
	if !antigravityHooksDisabled(rootDisabled) {
		t.Error("expected antigravityHooksDisabled=true")
	}

	rootEnabled := map[string]any{
		"gortex": map[string]any{
			"enabled": true,
		},
	}
	if antigravityHooksDisabled(rootEnabled) {
		t.Error("expected antigravityHooksDisabled=false")
	}
}
