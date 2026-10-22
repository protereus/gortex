package antigravity

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

const (
	hookPluginName     = "gortex"
	hookTimeoutSeconds = 10
	hookAgentFlag      = "antigravity"
)

// HookEvents are the canonical PascalCase lifecycle events this adapter
// installs, in the order they matter for adoption and `gortex doctor`.
//
// Antigravity's per-turn prompt hook is named "PreInvocation" on disk;
// antigravityHookEventNames translates between Gortex's canonical
// "UserPromptSubmit" telemetry bucket and the on-disk key so doctor's
// tally and internal/hooks' effectiveness allowlist stay unified.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

var antigravityHookEventNames = map[string]string{
	"SessionStart":     "SessionStart",
	"UserPromptSubmit": "PreInvocation",
	"PreToolUse":       "PreToolUse",
	"PostToolUse":      "PostToolUse",
	"Stop":             "Stop",
}

// HookConfigPath returns the user-level hooks.json path read by
// Antigravity CLI (agy) and Antigravity 2.0 (~/.gemini/config/hooks.json).
func HookConfigPath(home string) string {
	if home == "" {
		return ""
	}
	if override := antigravityConfigHomeOverride(home); override != "" {
		return filepath.Join(override, "hooks.json")
	}
	return filepath.Join(home, ".gemini", "config", "hooks.json")
}

func hookBinary(env agents.Env) string {
	if base := strings.TrimSpace(env.HookCommand); base != "" {
		if bin, ok := strings.CutSuffix(base, " hook"); ok {
			return strings.TrimSpace(bin)
		}
		return base
	}
	return agents.ResolveGortexHookBinary()
}

func posixQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`&;|<>()*?[]{}#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func hookCommandForEvent(env agents.Env, onDiskEvent string) string {
	bin := posixQuote(strings.ReplaceAll(hookBinary(env), `\`, "/"))
	base := bin + " hook --agent=" + hookAgentFlag + " --event=" + onDiskEvent
	switch strings.ToLower(strings.TrimSpace(env.HookMode)) {
	case "enrich":
		return base + " --mode=enrich"
	case "consult-unlock":
		return base + " --mode=consult-unlock"
	case "nudge", "adaptive-nudge":
		return base + " --mode=nudge"
	default:
		return base
	}
}

func desiredHandler(env agents.Env, onDiskEvent string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": hookCommandForEvent(env, onDiskEvent),
		"timeout": hookTimeoutSeconds,
	}
}

func desiredEventValue(env agents.Env, onDiskEvent string) any {
	handler := desiredHandler(env, onDiskEvent)
	if onDiskEvent == "PreToolUse" || onDiskEvent == "PostToolUse" {
		return []any{
			map[string]any{
				"matcher": "*",
				"hooks":   []any{handler},
			},
		}
	}
	return []any{handler}
}

// upsertAntigravityHooks merges the "gortex" hook spec into root
// (~/.gemini/config/hooks.json). Unrelated top-level hook specs are
// preserved untouched, and an existing `"enabled": false` kill-switch
// inside the "gortex" spec is respected unless opts.Force is set.
func upsertAntigravityHooks(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	changed := false
	spec, ok := root[hookPluginName].(map[string]any)
	if !ok {
		if _, exists := root[hookPluginName]; exists && !opts.Force {
			return false
		}
		spec = make(map[string]any)
		changed = true
	}

	for _, canonicalEvent := range HookEvents {
		onDiskEvent := antigravityHookEventNames[canonicalEvent]
		want := desiredEventValue(env, onDiskEvent)
		existing, exists := spec[onDiskEvent]
		if !exists || opts.Force || !agents.MCPEntriesEqual(existing, want) {
			spec[onDiskEvent] = want
			changed = true
		}
	}

	if changed {
		root[hookPluginName] = spec
	}
	return changed
}

func isGortexAntigravityCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	if !strings.Contains(lower, "gortex") || !strings.Contains(lower, "hook") {
		return false
	}
	return strings.Contains(lower, "--agent="+hookAgentFlag) || strings.Contains(lower, "--agent "+hookAgentFlag)
}

func countGortexEventHandlers(onDiskEvent string, raw any) int {
	list, ok := antigravityHookSlice(raw)
	if !ok {
		return 0
	}
	count := 0
	if onDiskEvent == "PreToolUse" || onDiskEvent == "PostToolUse" {
		for _, item := range list {
			group, ok := item.(map[string]any)
			if !ok {
				continue
			}
			handlers, ok := antigravityHookSlice(group["hooks"])
			if !ok {
				continue
			}
			for _, h := range handlers {
				if hm, ok := h.(map[string]any); ok {
					if cmd, _ := hm["command"].(string); isGortexAntigravityCommand(cmd) {
						count++
					}
				}
			}
		}
		return count
	}
	for _, item := range list {
		if hm, ok := item.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); isGortexAntigravityCommand(cmd) {
				count++
			}
		}
	}
	return count
}

func antigravityHookSlice(v any) ([]any, bool) {
	if v == nil {
		return nil, true
	}
	switch list := v.(type) {
	case []any:
		return list, true
	case []map[string]any:
		out := make([]any, 0, len(list))
		for _, item := range list {
			out = append(out, item)
		}
		return out, true
	default:
		return nil, false
	}
}
