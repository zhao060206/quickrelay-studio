package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOutpostToolRegistryAliasTruncateAndDecode(t *testing.T) {
	longDesc := strings.Repeat("工具说明文档🦀", 1200) + "\nEND_OF_LONG_TOOL_DOCS"
	req := &ChatRequest{
		Tools: []Tool{
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "PowerShell-Run",
					Description: longDesc,
					Parameters: map[string]any{
						"type":       "object",
						"properties": map[string]any{"script": map[string]any{"type": "string"}},
					},
				},
			},
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "Read",
					Description: "Short read description",
					Parameters: map[string]any{
						"type":       "object",
						"properties": map[string]any{"file_path": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}

	reg := newOutpostToolRegistry(req)
	reg2 := newOutpostToolRegistry(req)
	if reg.ToolsHash != reg2.ToolsHash {
		t.Fatalf("toolsHash mismatch across identical requests: %s vs %s", reg.ToolsHash, reg2.ToolsHash)
	}
	if len(reg.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(reg.Tools))
	}

	psTool := reg.Tools[0]
	if len(psTool.Description) > outpostMaxToolDescBytes {
		t.Fatalf("registered description exceeds %d bytes: %d", outpostMaxToolDescBytes, len(psTool.Description))
	}
	if !utf8.ValidString(psTool.Description) {
		t.Fatalf("truncated registered description is not valid UTF-8")
	}
	if !strings.Contains(reg.PromptDocs, "END_OF_LONG_TOOL_DOCS") {
		t.Fatalf("PromptDocs lost trailing documentation")
	}

	devName := outpostDeviceName("thread-12345")
	modelName := outpostModelToolName(devName, "PowerShell-Run")
	if len(modelName) > 64 {
		t.Fatalf("model tool name exceeds 64 chars: %d (%s)", len(modelName), modelName)
	}

	readAlias := reg.OrigToAlias["Read"]
	wrappedArgs := json.RawMessage(`{"params":{"name":"` + readAlias + `","arguments":{"file_path":"src/main.go","nested":{"ok":true}},"_quickwork":{"result_transport_version":1,"token":"secret-internal"}}}`)
	origName, decodedArgs, err := reg.decodeCall(outpostModelToolName(devName, "Read"), wrappedArgs, devName)
	if err != nil {
		t.Fatalf("decodeCall wrapped failed: %v", err)
	}
	if origName != "Read" {
		t.Fatalf("origName = %q, want Read", origName)
	}
	if strings.Contains(decodedArgs, "_quickwork") || strings.Contains(decodedArgs, "secret-internal") {
		t.Fatalf("decoded args leaked transport wrapper: %s", decodedArgs)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(decodedArgs), &parsed); err != nil {
		t.Fatalf("decoded args invalid JSON: %v (%s)", err, decodedArgs)
	}
	if parsed["file_path"] != "src/main.go" {
		t.Fatalf("unexpected decoded file_path: %v", parsed)
	}

	if _, _, err := reg.decodeCall("unknown_tool", wrappedArgs, devName); err == nil {
		t.Fatalf("expected error for unknown tool name")
	}
}

func TestBuildOutpostHistoryEventsAndTurnSplit(t *testing.T) {
	devName := outpostDeviceName("thread-hist-1")
	msgs := []Message{
		{Role: "system", Content: "System rule 1"},
		{Role: "user", Content: "Read a.txt and check"},
		{
			Role:    "assistant",
			Content: "Let me check.\n[assistant]\n",
			ToolCalls: []ToolCall{
				{
					ID:   "call_a1",
					Type: "function",
					Function: FunctionCall{
						Name:      "Read",
						Arguments: `{"file_path":"a.txt"}`,
					},
				},
			},
		},
		{Role: "tool", ToolCallID: "call_a1", Content: "[tool error] permission denied"},
		{Role: "assistant", Content: "It failed, trying another."},
		{Role: "user", Content: "Now read b.txt"},
	}

	turnIn, err := extractOutpostTurnInput(msgs)
	if err != nil {
		t.Fatalf("extractOutpostTurnInput: %v", err)
	}
	if turnIn.SystemPrompt != "System rule 1" {
		t.Fatalf("SystemPrompt = %q", turnIn.SystemPrompt)
	}
	if turnIn.TurnUserText != "Now read b.txt" {
		t.Fatalf("TurnUserText = %q", turnIn.TurnUserText)
	}
	if len(turnIn.HistoryMessages) != 4 {
		t.Fatalf("HistoryMessages len = %d, want 4", len(turnIn.HistoryMessages))
	}

	events, err := buildOutpostHistoryEvents(turnIn.HistoryMessages, "thread-hist-1", devName)
	if err != nil {
		t.Fatalf("buildOutpostHistoryEvents: %v", err)
	}
	var sawStart, sawArgs, sawErrResult, sawProjection bool
	for _, ev := range events {
		tp, _ := ev["type"].(string)
		data, _ := ev["data"].(map[string]any)
		switch tp {
		case "tool_start":
			sawStart = data["tool_name"] == outpostModelToolName(devName, "Read")
		case "tool_arguments":
			args, _ := data["arguments"].(map[string]any)
			sawArgs = args["file_path"] == "a.txt"
		case "message_assistant_complete":
			if fc, _ := data["full_content"].(string); strings.Contains(fc, "[assistant]") {
				t.Fatalf("assistant complete kept internal marker: %q", fc)
			}
		case "tool_result":
			sawErrResult = data["status"] == "error" && data["result"] == "permission denied"
		case "tool_result_projection":
			sawProjection = data["status"] == "error"
		}
	}
	if !sawStart || !sawArgs || !sawErrResult || !sawProjection {
		t.Fatalf("missing expected imported events: start=%v args=%v err=%v proj=%v", sawStart, sawArgs, sawErrResult, sawProjection)
	}

	// 孤立 tool_result 与含图片消息应拒绝结构化导入以便自动降级
	orphan := []Message{
		{Role: "tool", ToolCallID: "call_missing", Content: "orphan"},
	}
	if _, err := buildOutpostHistoryEvents(orphan, "t2", devName); err == nil {
		t.Fatalf("expected error for orphan tool_result")
	}
	imgHistory := []Message{
		{Role: "user", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}}}},
	}
	if _, err := buildOutpostHistoryEvents(imgHistory, "t3", devName); err == nil {
		t.Fatalf("expected error for image message in outpost history")
	}
}

func TestQuickOutputCleansRoleMarkersOutsideCodeBlocks(t *testing.T) {
	in := "[assistant]\nHere is your answer:\n```text\n[assistant]\nkeep this inside fence\n```\n[main]\nDone."
	out, _ := cleanQuickOutput(in, nil, nil)
	if strings.Contains(out, "[assistant]\nHere") || strings.Contains(out, "[main]\nDone") {
		t.Fatalf("expected top-level role markers stripped, got %q", out)
	}
	if !strings.Contains(out, "```text\n[assistant]\nkeep this inside fence\n```") {
		t.Fatalf("fenced code block marker was modified: %q", out)
	}
}

func TestOutpostToolRegistry_ResolveNameExactMatching(t *testing.T) {
	longToolName := "ExtremelyLongCustomWorkspaceInspectorToolNameThatMustBeHashTruncatedAt64Chars"
	req := &ChatRequest{
		Tools: []Tool{
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "AlphaTool",
					Description: "Fictional alpha tool",
					Parameters:  map[string]any{"type": "object"},
				},
			},
			{
				Type: "function",
				Function: ToolFunction{
					Name:        longToolName,
					Description: "Fictional long tool",
					Parameters:  map[string]any{"type": "object"},
				},
			},
		},
	}

	reg := newOutpostToolRegistry(req)
	devName := outpostDeviceName("thread-device-a")
	otherDevName := outpostDeviceName("thread-device-b")

	alphaAlias := reg.OrigToAlias["AlphaTool"]
	longAlias := reg.OrigToAlias[longToolName]
	alphaFull := outpostModelToolNameForAlias(devName, alphaAlias)
	longTruncated := outpostModelToolNameForAlias(devName, longAlias)
	if len(longTruncated) > 64 {
		t.Fatalf("truncated name exceeded 64 chars: %d", len(longTruncated))
	}

	cases := []struct {
		name     string
		wireName string
		wantOrig string
		wantOK   bool
	}{
		{
			name:     "registered_short_alias",
			wireName: alphaAlias,
			wantOrig: "AlphaTool",
			wantOK:   true,
		},
		{
			name:     "own_device_full_name",
			wireName: alphaFull,
			wantOrig: "AlphaTool",
			wantOK:   true,
		},
		{
			name:     "own_device_truncated_64_name",
			wireName: longTruncated,
			wantOrig: longToolName,
			wantOK:   true,
		},
		{
			name:     "other_device_prefix_rejected",
			wireName: strings.ReplaceAll(otherDevName, " ", "_") + "_" + alphaAlias,
			wantOK:   false,
		},
		{
			name:     "other_device_truncated_rejected",
			wireName: outpostModelToolNameForAlias(otherDevName, longAlias),
			wantOK:   false,
		},
		{
			name:     "arbitrary_extra_prefix_rejected",
			wireName: "evil_prefix_" + alphaAlias,
			wantOK:   false,
		},
		{
			name:     "tampered_hash_truncated_rejected",
			wireName: "t_00000000_" + alphaAlias,
			wantOK:   false,
		},
		{
			name:     "unknown_tool_rejected",
			wireName: "BetaTool",
			wantOK:   false,
		},
		{
			name:     "empty_rejected",
			wireName: "   ",
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOrig, gotOK := reg.resolveName(tc.wireName, devName)
			if gotOK != tc.wantOK || gotOrig != tc.wantOrig {
				t.Fatalf("resolveName(%q) = (%q, %v), want (%q, %v)", tc.wireName, gotOrig, gotOK, tc.wantOrig, tc.wantOK)
			}
			if _, _, decErr := reg.decodeCall(tc.wireName, json.RawMessage(`{}`), devName); (decErr == nil) != tc.wantOK {
				t.Fatalf("decodeCall(%q) err=%v, wantOK=%v", tc.wireName, decErr, tc.wantOK)
			}
		})
	}
}
