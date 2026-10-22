package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/localizationauth"
	"github.com/zzet/gortex/internal/modelhint"
)

func TestUnwrapAntigravityTool(t *testing.T) {
	// 1. Lazy call_mcp_tool targeting gortex -> mcp__gortex__<op>.
	raw, mapped, args := unwrapAntigravityTool("call_mcp_tool", map[string]any{
		"ServerName": "gortex",
		"ToolName":   "explore",
		"Arguments": map[string]any{
			"task": "trace auth flow",
		},
	})
	if raw != "mcp__gortex__explore" || mapped != "mcp__gortex__explore" {
		t.Fatalf("unwrap call_mcp_tool = (%q, %q), want mcp__gortex__explore", raw, mapped)
	}
	if args["task"] != "trace auth flow" {
		t.Fatalf("unwrapped args = %v", args)
	}

	// 2. Eager mcp_gortex_<op> -> mcp__gortex__<op>.
	raw, mapped, _ = unwrapAntigravityTool("mcp_gortex_search", map[string]any{"query": "foo"})
	if raw != "mcp__gortex__search" || mapped != "mcp__gortex__search" {
		t.Fatalf("unwrap mcp_gortex_search = (%q, %q)", raw, mapped)
	}

	// 3. Built-in Antigravity tool names + argument aliases.
	_, mapped, args = unwrapAntigravityTool("read_file", map[string]any{
		"Path":      "internal/auth/token.go",
		"StartLine": 10,
		"EndLine":   40,
	})
	if mapped != "Read" {
		t.Fatalf("read_file mapped to %q, want Read", mapped)
	}
	if args["file_path"] != "internal/auth/token.go" || args["offset"] != 10 || args["limit"] != 40 {
		t.Fatalf("read_file aliased args = %v", args)
	}
}

func TestHandleAntigravitySessionStartInjectsSystemMessageAndWritesModelHint(t *testing.T) {
	hintDir := t.TempDir()
	t.Setenv("GORTEX_MODEL_HINT_DIR", hintDir)

	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "1.0.0", Ready: true,
			TrackedRepos: []daemon.TrackedRepoStatus{{Name: "repo", Path: repoTmpFixtureRoot, Workspace: "repo", Nodes: 10}},
			Workspaces:   []daemon.WorkspaceSummary{{Slug: "repo"}},
		}, nil
	})

	payload := copilotPayload(t, map[string]any{
		"conversationId": "conv-123",
		"workspacePaths": []string{repoTmpFixtureRoot},
		"modelName":      "gemini-3.1-pro",
	})
	out, code := handleAntigravity(payload, 0, ModeDeny, "SessionStart")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}

	var resp antigravityInjectResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal SessionStart response: %v\n%s", err, out)
	}
	if len(resp.InjectSteps) != 1 || resp.InjectSteps[0].SystemMessage == nil {
		t.Fatalf("expected 1 systemMessage step, got %+v", resp)
	}
	if !strings.Contains(resp.InjectSteps[0].SystemMessage.SystemMessage, "Gortex Session Orientation") {
		t.Errorf("unexpected systemMessage: %s", resp.InjectSteps[0].SystemMessage.SystemMessage)
	}

	hint, ok := modelhint.Read(repoTmpFixtureRoot)
	if !ok {
		t.Fatal("expected modelhint to be written on SessionStart")
	}
	if hint.Model != "gemini-3.1-pro" || hint.Client != "antigravity" {
		t.Errorf("unexpected hint: %+v", hint)
	}
}

func TestHandleAntigravityPreInvocationInjectsEphemeralMessageFromTranscript(t *testing.T) {
	withDaemonReachable(t, true)
	stubPromptProbe(t, []grepSymbolHit{
		{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
	})

	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := `{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\nfix ValidateToken in token.go\n</USER_REQUEST>"}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := copilotPayload(t, map[string]any{
		"conversationId": "conv-456",
		"workspacePaths": []string{"/repo"},
		"transcriptPath": transcript,
		"invocationNum":  1,
	})
	out, code := handleAntigravity(payload, 0, ModeDeny, "PreInvocation")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}

	var resp antigravityInjectResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal PreInvocation response: %v\n%s", err, out)
	}
	if len(resp.InjectSteps) != 1 || !strings.Contains(resp.InjectSteps[0].EphemeralMessage, "ValidateToken") {
		t.Fatalf("expected EphemeralMessage with ValidateToken, got %+v", resp)
	}
}

func TestHandleAntigravityPreToolUseDeniesWholeFileReadAndUnlocksAfterCallMCPTool(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 9)

	// 1. Before consulting Gortex, a whole-file `read_file` in ModeConsultUnlock is denied.
	readPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-unlock",
		"workspacePaths": []string{"/repo"},
		"toolCall": map[string]any{
			"name": "read_file",
			"args": map[string]any{"Path": "internal/auth/token.go"},
		},
	})
	out, _ := handleAntigravity(readPayload, 0, ModeConsultUnlock, "PreToolUse")
	var dec antigravityDecisionResponse
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("unmarshal decision: %v\n%s", err, out)
	}
	if dec.Decision != "deny" || !strings.Contains(dec.Reason, "9 symbols indexed") {
		t.Fatalf("expected deny before Gortex consultation, got %+v", dec)
	}

	// 2. Calling Gortex via `call_mcp_tool` marks the session as graph-consulted.
	mcpPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-unlock",
		"workspacePaths": []string{"/repo"},
		"toolCall": map[string]any{
			"name": "call_mcp_tool",
			"args": map[string]any{
				"ServerName": "gortex",
				"ToolName":   "explore",
				"Arguments":  map[string]any{"task": "check token"},
			},
		},
	})
	out, _ = handleAntigravity(mcpPayload, 0, ModeConsultUnlock, "PreToolUse")
	if out != `{"decision":"allow"}` {
		t.Fatalf("expected {\"decision\":\"allow\"} for call_mcp_tool explore, got %s", out)
	}

	// 3. Subsequent `read_file` in ModeConsultUnlock is now allowed.
	out, _ = handleAntigravity(readPayload, 0, ModeConsultUnlock, "PreToolUse")
	if out != `{"decision":"allow"}` {
		t.Fatalf("expected {\"decision\":\"allow\"} after Gortex consultation in ModeConsultUnlock, got %s", out)
	}
}

func TestHandleAntigravityLocalizationTerminalLifecycleWithOverwriteAndStop(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)
	stubPromptProbe(t, nil)

	// 1. PreInvocation starts the turn with HookArgsCommon.lastUserInput.
	preInvPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-term",
		"workspacePaths": []string{"/repo"},
		"lastUserInput":  "<USER_REQUEST>\n--- problem statement ---\nwhere is ValidateToken defined in the auth package?\n-------------------------\n</USER_REQUEST>",
		"invocationNum":  1,
	})
	_, code := handleAntigravity(preInvPayload, 0, ModeDeny, "PreInvocation")
	if code != 0 {
		t.Fatalf("PreInvocation code = %d", code)
	}

	// 2. PreToolUse for call_mcp_tool(gortex.explore localize) returns decision=allow
	//    AND overwrite.Arguments containing _gortex_localization_auth + rewritten task.
	preToolPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-term",
		"workspacePaths": []string{"/repo"},
		"stepIdx":        3,
		"toolCall": map[string]any{
			"name": "call_mcp_tool",
			"args": map[string]any{
				"ServerName": "gortex",
				"ToolName":   "explore",
				"Arguments": map[string]any{
					"operation": "localize",
					"task":      "short query",
					"options":   map[string]any{"new_user_task": true},
				},
			},
		},
	})
	out, _ := handleAntigravity(preToolPayload, 0, ModeDeny, "PreToolUse")
	var preDec antigravityDecisionResponse
	if err := json.Unmarshal([]byte(out), &preDec); err != nil {
		t.Fatalf("unmarshal PreToolUse decision: %v\n%s", err, out)
	}
	if preDec.Decision != "allow" {
		t.Fatalf("expected decision=allow, got %+v", preDec)
	}
	overwrittenArgs, _ := preDec.Overwrite["Arguments"].(map[string]any)
	authToken, _ := overwrittenArgs[localizationauth.ArgumentKey].(string)
	if strings.TrimSpace(authToken) == "" {
		t.Fatalf("expected _gortex_localization_auth in overwrite.Arguments, got %+v", preDec.Overwrite)
	}
	if overwrittenArgs["task"] != "where is ValidateToken defined in the auth package?" {
		t.Fatalf("expected task rewritten to problem statement, got %v", overwrittenArgs["task"])
	}

	// 3. Simulate the MCP server publishing the answer_ready receipt for authToken.
	if !localizationauth.Publish(authToken, localizationauth.Receipt{
		FinalResponse:   "ValidateToken is defined in internal/auth/token.go:42.",
		ContractVersion: 2,
		Enforceable:     true,
		PrimaryIDs:      []string{"internal/auth/token.go::ValidateToken#function"},
		EvidenceIDs:     []string{"internal/auth/token.go::ValidateToken#function"},
	}) {
		t.Fatal("failed to publish localizationauth receipt")
	}

	// 4. PostToolUse observes the completed explore(localize) call and arms the terminal marker.
	postToolPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-term",
		"workspacePaths": []string{"/repo"},
		"stepIdx":        3,
		"toolCall": map[string]any{
			"name": "call_mcp_tool",
			"args": map[string]any{
				"ServerName": "gortex",
				"ToolName":   "explore",
				"Arguments": map[string]any{
					"operation": "localize",
				},
			},
		},
		"result": `{"terminal":true}`,
	})
	out, _ = handleAntigravity(postToolPayload, 0, ModeDeny, "PostToolUse")
	if out != "{}" {
		t.Fatalf("expected {} from PostToolUse, got %s", out)
	}

	// 5. Subsequent PreToolUse in the same turn is denied by the armed terminal marker!
	followUpPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-term",
		"workspacePaths": []string{"/repo"},
		"stepIdx":        4,
		"toolCall": map[string]any{
			"name": "read_file",
			"args": map[string]any{"Path": "internal/auth/token.go"},
		},
	})
	out, _ = handleAntigravity(followUpPayload, 0, ModeDeny, "PreToolUse")
	var denyDec antigravityDecisionResponse
	if err := json.Unmarshal([]byte(out), &denyDec); err != nil {
		t.Fatalf("unmarshal follow-up PreToolUse decision: %v\n%s", err, out)
	}
	if denyDec.Decision != "deny" || !strings.Contains(denyDec.Reason, "ValidateToken is defined in internal/auth/token.go:42.") {
		t.Fatalf("expected terminal deny with final response, got %+v", denyDec)
	}

	// 6. Stop hook checks finalModelOutput against PrimaryIDs and returns decision=continue on ungrounded claims.
	stopPayload := copilotPayload(t, map[string]any{
		"conversationId":    "conv-term",
		"workspacePaths":    []string{"/repo"},
		"executionNum":      1,
		"terminationReason": "NO_TOOL_CALL",
		"finalModelOutput":  "The handler is in `UnrelatedHandler` inside `other/pkg/handler.go`.",
	})
	out, _ = handleAntigravity(stopPayload, 0, ModeDeny, "Stop")
	var stopDec antigravityDecisionResponse
	if err := json.Unmarshal([]byte(out), &stopDec); err != nil {
		t.Fatalf("unmarshal Stop decision: %v\n%s", err, out)
	}
	if stopDec.Decision != "continue" || !strings.Contains(stopDec.Reason, "ValidateToken") {
		t.Fatalf("expected Stop hook to return decision=continue for ungrounded claim, got %+v", stopDec)
	}
}

func TestAntigravityJSONEscapedArgsUnquoted(t *testing.T) {
	_, mapped, args := unwrapAntigravityTool("run_command", map[string]any{
		"CommandLine": `"grep \"foo\" pkg/a.go"`,
	})
	if mapped != "Bash" {
		t.Fatalf("mapped = %q, want Bash", mapped)
	}
	if args["command"] != `grep "foo" pkg/a.go` {
		t.Fatalf("unquoted command = %q, want %q", args["command"], `grep "foo" pkg/a.go`)
	}

	_, mappedRead, argsRead := unwrapAntigravityTool("read_file", map[string]any{
		"Path": `"internal/auth/token.go"`,
	})
	if mappedRead != "Read" {
		t.Fatalf("mappedRead = %q, want Read", mappedRead)
	}
	if argsRead["file_path"] != "internal/auth/token.go" {
		t.Fatalf("unquoted file_path = %q, want internal/auth/token.go", argsRead["file_path"])
	}
}

func TestRunAntigravityStdinStdout(t *testing.T) {
	origStdin := os.Stdin
	origStdout := os.Stdout
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	payload := `{"conversationId":"conv-run","workspacePaths":["/tmp"],"toolCall":{"name":"read_file","args":{"Path":"foo.go"}}}` + "\n"
	go func() {
		_, _ = inW.Write([]byte(payload))
		_ = inW.Close()
	}()

	os.Stdin = inR
	os.Stdout = outW

	RunAntigravity(0, ModeDeny, "PreToolUse")
	_ = outW.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, outR)
	output := buf.String()
	if !strings.Contains(output, `"decision":"allow"`) {
		t.Errorf("RunAntigravity stdout = %q, want decision:allow", output)
	}
}

func TestNormalizeAntigravityEvents(t *testing.T) {
	// 1. Explicit names
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"sessionstart", "SessionStart"},
		{"userpromptsubmit", "PreInvocation"},
		{"userpromptsubmitted", "PreInvocation"},
		{"preinvocation", "PreInvocation"},
		{"pretooluse", "PreToolUse"},
		{"posttooluse", "PostToolUse"},
		{"aftertool", "AfterTool"},
		{"stop", "Stop"},
		{"agentstop", "Stop"},
	} {
		if got := normalizeAntigravityEvent(tc.input, antigravityInput{}); got != tc.want {
			t.Errorf("normalizeAntigravityEvent(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	// 2. Inferred from payload shape
	// Tool call with result -> PostToolUse
	inPost := antigravityInput{
		ToolName: "read_file",
		Result:   json.RawMessage(`{"bytes": 10}`),
	}
	if got := normalizeAntigravityEvent("", inPost); got != "PostToolUse" {
		t.Errorf("inferred tool result = %q, want PostToolUse", got)
	}

	// Tool call without result -> PreToolUse
	inPre := antigravityInput{
		ToolName: "read_file",
	}
	if got := normalizeAntigravityEvent("", inPre); got != "PreToolUse" {
		t.Errorf("inferred tool call = %q, want PreToolUse", got)
	}

	// Stop payload
	inStop := antigravityInput{
		TerminationReason: "COMPLETED",
	}
	if got := normalizeAntigravityEvent("", inStop); got != "Stop" {
		t.Errorf("inferred terminationReason = %q, want Stop", got)
	}

	// Prompt payload -> PreInvocation
	inPrompt := antigravityInput{
		Prompt: "hello",
	}
	if got := normalizeAntigravityEvent("", inPrompt); got != "PreInvocation" {
		t.Errorf("inferred prompt = %q, want PreInvocation", got)
	}
}

func TestAntigravityNativeToolAliases(t *testing.T) {
	// 1. Write / Edit tools
	for _, name := range []string{"write_file", "write_to_file", "edit_file", "replace_file_content", "multi_replace_file_content"} {
		_, mapped, args := unwrapAntigravityTool(name, map[string]any{"TargetFile": "pkg/b.go"})
		if mapped != "Write" && mapped != "Edit" {
			t.Errorf("%s mapped to %q, want Write or Edit", name, mapped)
		}
		if args["file_path"] != "pkg/b.go" {
			t.Errorf("%s file_path = %v", name, args["file_path"])
		}
	}

	// 2. Grep search
	_, mappedGrep, argsGrep := unwrapAntigravityTool("grep_search", map[string]any{
		"Query":      "MyClass",
		"SearchPath": "src",
	})
	if mappedGrep != "Grep" || argsGrep["pattern"] != "MyClass" || argsGrep["path"] != "src" {
		t.Errorf("grep_search mapping = (%s, %v)", mappedGrep, argsGrep)
	}

	// 3. Glob / find / list
	for _, name := range []string{"find_by_name", "list_dir"} {
		_, mappedGlob, argsGlob := unwrapAntigravityTool(name, map[string]any{
			"Pattern":         "*.go",
			"SearchDirectory": "cmd",
		})
		if mappedGlob != "Glob" || argsGlob["pattern"] != "*.go" || argsGlob["path"] != "cmd" {
			t.Errorf("%s mapping = (%s, %v)", name, mappedGlob, argsGlob)
		}
	}

	// 4. call_mcp_tool non-gortex
	rawOther, mappedOther, _ := unwrapAntigravityTool("call_mcp_tool", map[string]any{
		"ServerName": "sqlite",
		"ToolName":   "query",
		"Arguments":  map[string]any{"sql": "SELECT 1"},
	})
	if rawOther != "mcp__sqlite__query" || mappedOther != "mcp__sqlite__query" {
		t.Errorf("other mcp server = (%q, %q)", rawOther, mappedOther)
	}

	// 5. Lowercase arguments in call_mcp_tool overwrite
	overwrite := antigravityWrapOverwrite("call_mcp_tool", map[string]any{"arguments": map[string]any{}}, map[string]any{"task": "new"})
	if _, ok := overwrite["arguments"]; !ok {
		t.Errorf("expected lowercase arguments key preserved in overwrite, got %v", overwrite)
	}
}

func TestHandleAntigravityMalformedJSON(t *testing.T) {
	// PreToolUse with malformed JSON returns allow
	out, code := handleAntigravity([]byte("{malformed"), 0, ModeDeny, "PreToolUse")
	if code != 0 || !strings.Contains(out, `"decision":"allow"`) {
		t.Errorf("malformed PreToolUse = (%s, %d)", out, code)
	}

	// Other events return {}
	outOther, codeOther := handleAntigravity([]byte("{malformed"), 0, ModeDeny, "SessionStart")
	if codeOther != 0 || outOther != "{}" {
		t.Errorf("malformed SessionStart = (%s, %d)", outOther, codeOther)
	}
}

func TestAntigravityHelperEdgeCases(t *testing.T) {
	// 1. antigravityMapArg
	args := map[string]any{
		"strMap":  "{\"hello\": \"world\"}",
		"badMap":  "not-json",
		"realMap": map[string]any{"num": 42},
	}
	gotStr := antigravityMapArg(args, "strMap")
	if gotStr["hello"] != "world" {
		t.Errorf("gotStr: %v", gotStr)
	}
	gotReal := antigravityMapArg(args, "realMap")
	if gotReal["num"] != 42 {
		t.Errorf("gotReal: %v", gotReal)
	}
	gotBad := antigravityMapArg(args, "badMap", "missing")
	if len(gotBad) != 0 {
		t.Errorf("gotBad: %v", gotBad)
	}

	// 2. finalModelOutput
	inSnake := antigravityInput{FinalModelOutputSnake: "done via snake"}
	if inSnake.finalModelOutput() != "done via snake" {
		t.Errorf("inSnake: %s", inSnake.finalModelOutput())
	}
	inEmpty := antigravityInput{}
	if inEmpty.finalModelOutput() != "" {
		t.Errorf("inEmpty: %s", inEmpty.finalModelOutput())
	}

	// 3. model, toolUseID, cwd fallbacks
	inAliases := antigravityInput{
		Model:          "gemini-pro",
		ToolUseID:      "call-123",
		WorkspacePaths: []string{"/workspace"},
	}
	if inAliases.model() != "gemini-pro" {
		t.Errorf("model: %s", inAliases.model())
	}
	if inAliases.toolUseID() != "call-123" {
		t.Errorf("toolUseID: %s", inAliases.toolUseID())
	}
	if inAliases.cwd() != "/workspace" {
		t.Errorf("cwd: %s", inAliases.cwd())
	}
}

// A whole-file Gortex read is a nudge on Claude and Kimi, and a deny only
// under GORTEX_HOOK_FORCE_COMPRESS. PreToolUse has no soft channel, so the
// default answer is a plain allow rather than a deny.
func TestHandleAntigravityGortexWholeFileReadDeniesOnlyWhenForced(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)

	payload := copilotPayload(t, map[string]any{
		"conversationId": "conv-gortex-read",
		"workspacePaths": []string{"/repo"},
		"toolCall": map[string]any{
			"name": "call_mcp_tool",
			"args": map[string]any{
				"ServerName": "gortex",
				"ToolName":   "read_file",
				"Arguments":  map[string]any{"path": "internal/auth/token.go"},
			},
		},
	})

	out, _ := handleAntigravity(payload, 0, ModeDeny, "PreToolUse")
	if out != `{"decision":"allow"}` {
		t.Fatalf("whole-file Gortex read should be allowed by default, got %s", out)
	}

	t.Setenv("GORTEX_HOOK_FORCE_COMPRESS", "1")
	out, _ = handleAntigravity(payload, 0, ModeDeny, "PreToolUse")
	var dec antigravityDecisionResponse
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("unmarshal decision: %v\n%s", err, out)
	}
	if dec.Decision != "deny" || !strings.Contains(dec.Reason, "compress_bodies") {
		t.Fatalf("expected compress_bodies deny under GORTEX_HOOK_FORCE_COMPRESS, got %+v", dec)
	}
}

// In nudge mode a Gortex call is the symbolic move that resets the
// fallback streak, as it does for Claude and Copilot.
func TestHandleAntigravityNudgeModeResetsStreakOnGortexCall(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 9)

	readPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-nudge",
		"workspacePaths": []string{"/repo"},
		"toolCall": map[string]any{
			"name": "read_file",
			"args": map[string]any{"Path": "internal/auth/token.go"},
		},
	})
	gortexPayload := copilotPayload(t, map[string]any{
		"conversationId": "conv-nudge",
		"workspacePaths": []string{"/repo"},
		"toolCall": map[string]any{
			"name": "call_mcp_tool",
			"args": map[string]any{
				"ServerName": "gortex",
				"ToolName":   "explore",
				"Arguments":  map[string]any{"task": "check token"},
			},
		},
	})
	decision := func(payload []byte) string {
		t.Helper()
		out, _ := handleAntigravity(payload, 0, ModeAdaptiveNudge, "PreToolUse")
		var dec antigravityDecisionResponse
		if err := json.Unmarshal([]byte(out), &dec); err != nil {
			t.Fatalf("unmarshal decision: %v\n%s", err, out)
		}
		return dec.Decision
	}

	for i := 0; i < nudgeThreshold-1; i++ {
		if got := decision(readPayload); got != "allow" {
			t.Fatalf("read %d below the threshold = %q, want allow", i+1, got)
		}
	}
	if got := decision(gortexPayload); got != "allow" {
		t.Fatalf("Gortex call = %q, want allow", got)
	}
	for i := 0; i < nudgeThreshold-1; i++ {
		if got := decision(readPayload); got != "allow" {
			t.Fatalf("read %d after the Gortex call = %q, want allow (streak should have reset)", i+1, got)
		}
	}
	if got := decision(readPayload); got != "deny" {
		t.Fatalf("read at the threshold = %q, want the one-shot nudge deny", got)
	}
}

// Before hooks moved to ~/.gemini/config/hooks.json, Gortex wrote
// `gortex hook --agent antigravity` (no --event) into ~/.gemini/settings.json,
// which only Gemini CLI reads. Those entries must keep getting Gemini's
// hookSpecificOutput shape, not agy's injectSteps.
func TestHandleAntigravityLegacyGeminiSettingsEntryAnswersInGeminiShape(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "1.0.0", Ready: true,
			TrackedRepos: []daemon.TrackedRepoStatus{{Name: "repo", Path: repoTmpFixtureRoot, Workspace: "repo", Nodes: 10}},
			Workspaces:   []daemon.WorkspaceSummary{{Slug: "repo"}},
		}, nil
	})

	payload := copilotPayload(t, map[string]any{
		"hook_event_name": "SessionStart",
		"cwd":             repoTmpFixtureRoot,
	})
	out, code := handleAntigravity(payload, 0, ModeDeny, "")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if strings.Contains(out, "injectSteps") {
		t.Fatalf("legacy Gemini entry answered in agy's shape: %s", out)
	}
	var resp HookOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal legacy response: %v\n%s", err, out)
	}
	if resp.HookSpecificOutput == nil || resp.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(resp.HookSpecificOutput.AdditionalContext, "Gortex Session Orientation") {
		t.Fatalf("expected Gemini hookSpecificOutput SessionStart orientation, got %s", out)
	}
}
