package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type museTestWriteCloser struct{ bytes.Buffer }

func (*museTestWriteCloser) Close() error { return nil }

// The helper implements the stable MSP surface exported by Muse Code 1.1.1.
// It deliberately sends events before the turn/start acknowledgement.
func runMuseTestHelper() {
	scenario := os.Getenv("MUSE_MSP_TEST_HELPER")
	wantArgs := "serve --trust-workspace --sandbox-network enabled"
	if scenario == "network-policy" {
		wantArgs = "serve --trust-workspace --sandbox-network enabled --extra"
	}
	if strings.Join(os.Args[1:], " ") != wantArgs && !(scenario == "models" && strings.Join(os.Args[1:], " ") == "serve") {
		os.Exit(10)
	}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 4<<20)
	send := func(v any) {
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			os.Exit(11)
		}
	}
	sessionID := "01991eaa-0000-7000-8000-000000000001"
	turnID := ""
	approvalDecisions := 0
	emitCompletedTurn := func(notify func(string, map[string]any)) {
		notify("turn/completed", map[string]any{"turnId": "old-turn", "terminal": "completed"})
		notify("item/started", map[string]any{"item": map[string]any{"itemId": "a", "turnId": turnID, "kind": "agentMessage", "status": "inProgress", "revision": 1}})
		notify("item/delta", map[string]any{"itemId": "a", "delta": "ok"})
		notify("item/completed", map[string]any{"item": map[string]any{"itemId": "a", "turnId": turnID, "kind": "agentMessage", "status": "completed", "revision": 2, "text": "okay"}})
		notify("item/completed", map[string]any{"item": map[string]any{"itemId": "tool", "turnId": turnID, "kind": "toolCall", "status": "completed", "revision": 1, "tool": "read_file", "args": "{\"path\":\"a\"}", "visibleOutput": "data"}})
		notify("turn/completed", map[string]any{"turnId": turnID, "terminal": "completed", "error": map[string]any{"message": "model failed"}, "usage": map[string]any{"inputTokens": 12, "outputTokens": 3}})
	}
	for scan.Scan() {
		var f struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &f) != nil {
			os.Exit(12)
		}
		result := any(map[string]any{})
		notify := func(method string, params map[string]any) {
			params["sessionId"] = sessionID
			send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		}
		switch f.Method {
		case "model/list":
			result = map[string]any{"models": []any{map[string]any{"modelId": "muse-test", "displayLabel": "Muse Test", "providerId": "meta", "isDefault": true}}}
		case "initialize":
			if scenario == "handshake-timeout" {
				continue
			}
			result = map[string]any{"schema": map[string]any{"version": 1}}
		case "initialized":
			continue
		case "session/start", "session/resume":
			if scenario == "resume-missing" || scenario == "resume-auth" {
				kind := "sessionNotFound"
				if scenario == "resume-auth" {
					kind = "commandRejected"
				}
				send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "error": map[string]any{"code": -32000, "message": "cannot resume", "data": map[string]any{"kind": kind}}})
				continue
			}
			if f.Method == "session/resume" && f.Params["excludeItems"] != true {
				os.Exit(13)
			}
			if f.Method == "session/start" && f.Params["modelId"] != nil && f.Params["modelId"] != "test-model" {
				os.Exit(20)
			}
			if f.Method == "session/start" && f.Params["modelId"] != nil && f.Params["providerId"] != "meta" {
				os.Exit(21)
			}
			session := map[string]any{"sessionId": sessionID, "modelId": "test-model", "status": "idle", "activeTurnId": nil}
			if scenario == "resume-busy" {
				session["status"] = "running"
				session["activeTurnId"] = "old-turn"
			}
			if scenario == "resume-wrong" {
				session["sessionId"] = "wrong-session"
			}
			result = map[string]any{"session": session}
		case "session/setApprovalMode":
			if f.Params["mode"] != "allowAll" {
				os.Exit(16)
			}
			result = map[string]any{"status": "accepted"}
		case "approval/decide":
			if (scenario != "approval" && scenario != "approval-multi" && scenario != "approval-duplicate-update" && scenario != "approval-already-resolved") || f.Params["choiceId"] != "approvedForSession" {
				os.Exit(19)
			}
			approvalDecisions++
			if scenario == "approval-duplicate-update" && approvalDecisions > 1 {
				send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "error": map[string]any{"code": -32051, "message": "approval approval-1 is already resolved", "data": map[string]any{"kind": "approvalAlreadyResolved"}}})
				continue
			}
			if scenario == "approval-already-resolved" {
				send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "error": map[string]any{"code": -32051, "message": "approval approval-1 is already resolved", "data": map[string]any{"kind": "approvalAlreadyResolved"}}})
				emitCompletedTurn(notify)
				continue
			}
			terminal := scenario == "approval" || approvalDecisions == 2
			if scenario == "approval-duplicate-update" {
				terminal = true
			}
			result = map[string]any{"status": "accepted", "approvalId": "approval-1", "commandId": f.Params["commandId"], "terminal": terminal}
			if scenario == "approval-multi" && approvalDecisions == 1 {
				send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result})
				notify("approval/updated", map[string]any{
					"approvalId":           "approval-1",
					"currentRequirementId": map[string]any{"approvalId": "approval-1", "sourceIndex": 6},
					"availableChoices": []any{
						map[string]any{"choiceId": "approvedForSession", "decision": "approvedForSession", "scope": "session"},
						map[string]any{"choiceId": "denied", "decision": "denied", "scope": "once"},
					},
				})
				continue
			}
			if scenario == "approval-multi" {
				emitCompletedTurn(notify)
			}
			if scenario == "approval-duplicate-update" {
				send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result})
				notify("approval/updated", map[string]any{
					"approvalId":           "approval-1",
					"currentRequirementId": map[string]any{"approvalId": "approval-1", "sourceIndex": 2},
					"availableChoices": []any{
						map[string]any{"choiceId": "approvedForSession", "decision": "approvedForSession", "scope": "session"},
					},
				})
				emitCompletedTurn(notify)
				continue
			}
		case "session/setModel":
			model, _ := f.Params["model"].(map[string]any)
			if model["modelId"] != "test-model" || model["providerId"] != "meta" {
				os.Exit(22)
			}
			result = map[string]any{"status": "accepted"}
		case "turn/start":
			if f.Params["ifBusy"] != "queue" {
				os.Exit(17)
			}
			turnID, _ = f.Params["commandId"].(string)
			if scenario == "approval" || scenario == "approval-multi" || scenario == "approval-duplicate-update" || scenario == "approval-already-resolved" {
				notify("approval/requested", map[string]any{
					"approvalId":           "approval-1",
					"sessionId":            sessionID,
					"currentRequirementId": map[string]any{"approvalId": "approval-1", "sourceIndex": 2},
					"availableChoices": []any{
						map[string]any{"choiceId": "approvedForSession", "decision": "approvedForSession", "scope": "session"},
						map[string]any{"choiceId": "denied", "decision": "denied", "scope": "once"},
					},
				})
				result = map[string]any{"turnId": turnID, "status": "accepted", "disposition": "started"}
				if scenario == "approval-multi" || scenario == "approval-duplicate-update" || scenario == "approval-already-resolved" {
					send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result})
					continue
				}
			}
			parts, _ := f.Params["input"].([]any)
			if len(parts) != 1 || parts[0].(map[string]any)["text"] != "hello\nworld" {
				os.Exit(14)
			}
			if scenario == "eof" {
				return
			}
			if scenario == "malformed" {
				fmt.Println("not json")
				return
			}
			notify("turn/completed", map[string]any{"turnId": "old-turn", "terminal": "completed"})
			notify("item/started", map[string]any{"item": map[string]any{"itemId": "a", "turnId": turnID, "kind": "agentMessage", "status": "inProgress", "revision": 1}})
			notify("item/delta", map[string]any{"itemId": "a", "delta": "ok"})
			notify("item/completed", map[string]any{"item": map[string]any{"itemId": "a", "turnId": turnID, "kind": "agentMessage", "status": "completed", "revision": 2, "text": "okay"}})
			notify("item/completed", map[string]any{"item": map[string]any{"itemId": "tool", "turnId": turnID, "kind": "toolCall", "status": "completed", "revision": 1, "tool": "read_file", "args": "{\"path\":\"a\"}", "visibleOutput": "data"}})
			result = map[string]any{"turnId": turnID, "status": "accepted", "disposition": "started"}
			if scenario != "cancel" {
				terminal := "completed"
				if scenario == "failed" {
					terminal = "failed"
				}
				notify("turn/completed", map[string]any{"turnId": turnID, "terminal": terminal, "error": map[string]any{"message": "model failed"}, "usage": map[string]any{"inputTokens": 12, "outputTokens": 3}})
			}
		case "turn/interrupt":
			send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": map[string]any{"status": "accepted", "turnId": turnID}})
			time.Sleep(50 * time.Millisecond)
			if err := os.WriteFile(os.Getenv("MULTICA_MUSE_CANCEL_MARKER"), []byte("settled"), 0600); err != nil {
				os.Exit(18)
			}
			notify("turn/completed", map[string]any{"turnId": turnID, "terminal": "cancelled"})
			continue
		default:
			os.Exit(15)
		}
		send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result})
	}
}

func TestMuseMSPExecution(t *testing.T) {
	for _, scenario := range []string{"success", "resume", "approval", "approval-multi", "approval-duplicate-update", "approval-already-resolved", "resume-missing", "resume-auth", "resume-busy", "resume-wrong", "handshake-timeout", "eof", "malformed", "failed", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			marker := t.TempDir() + "/cancelled"
			backend, err := New("muse", Config{ExecutablePath: self, Env: map[string]string{"MUSE_MSP_TEST_HELPER": scenario, "MULTICA_MUSE_CANCEL_MARKER": marker}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			opts := ExecOptions{Cwd: t.TempDir(), Model: "test-model", ModelProvider: "meta", ThinkingLevel: "high", TurnInterruptTimeout: 300 * time.Millisecond}
			if scenario == "handshake-timeout" {
				opts.HandshakeTimeout = 50 * time.Millisecond
			}
			if strings.HasPrefix(scenario, "resume") {
				opts.ResumeSessionID = "01991eaa-0000-7000-8000-000000000001"
			}
			s, err := backend.Execute(ctx, "hello\nworld", opts)
			if err != nil {
				t.Fatal(err)
			}
			var text string
			var tools, results int
			for m := range s.Messages {
				if m.Type == MessageText {
					text += m.Content
					if scenario == "cancel" {
						cancel()
					}
				}
				if m.Type == MessageToolUse {
					tools++
				}
				if m.Type == MessageToolResult {
					results++
				}
			}
			r := <-s.Result
			if scenario == "cancel" {
				if _, err := os.Stat(marker); err != nil {
					t.Fatal("Muse killed before cancellation settled")
				}
			}
			want := "failed"
			if scenario == "success" || scenario == "resume" || scenario == "approval" || scenario == "approval-multi" || scenario == "approval-duplicate-update" || scenario == "approval-already-resolved" {
				want = "completed"
			}
			if scenario == "cancel" {
				want = "cancelled"
			}
			if r.Status != want {
				t.Fatalf("result = %+v, want %s", r, want)
			}
			if want == "completed" && (r.Output != "okay" || text != "okay" || tools != 1 || results != 1 || r.Usage["test-model"].InputTokens != 12) {
				t.Fatalf("result=%+v text=%q tools=%d results=%d", r, text, tools, results)
			}
			if r.ResumeRejectedTransient != (scenario == "resume-busy") {
				t.Fatalf("transient resume classification = %+v", r)
			}
			if r.ResumeRejected != (scenario == "resume-missing") {
				t.Fatalf("resume classification = %+v", r)
			}
		})
	}
}

func TestMuseNestedApprovalPreservesOuterRPCResponse(t *testing.T) {
	c := &museClient{
		stdin:  &museTestWriteCloser{},
		frames: make(chan museRead, 3),
	}
	approvalParams, err := json.Marshal(map[string]any{
		"approvalId":           "approval-1",
		"sessionId":            "session-1",
		"currentRequirementId": map[string]any{"approvalId": "approval-1", "sourceIndex": 2},
		"availableChoices": []any{
			map[string]any{"choiceId": "approvedForSession", "decision": "approvedForSession", "scope": "session"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.frames <- museRead{frame: museFrame{JSONRPC: "2.0", Method: "approval/requested", Params: approvalParams}}
	c.frames <- museRead{frame: museFrame{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: json.RawMessage(`{"status":"accepted"}`)}}
	c.frames <- museRead{frame: museFrame{JSONRPC: "2.0", ID: json.RawMessage("2"), Result: json.RawMessage(`{"status":"accepted","terminal":true}`)}}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := c.call(ctx, "turn/start", map[string]any{}, nil); err != nil {
		t.Fatalf("outer RPC response was lost while deciding approval: %v", err)
	}
}

func TestMuseDuplicateApprovalUpdateIsIgnored(t *testing.T) {
	stdin := &museTestWriteCloser{}
	c := &museClient{
		stdin:  stdin,
		frames: make(chan museRead, 1),
	}
	params, err := json.Marshal(map[string]any{
		"approvalId":           "approval-1",
		"sessionId":            "session-1",
		"currentRequirementId": map[string]any{"approvalId": "approval-1", "sourceIndex": 2},
		"availableChoices": []any{
			map[string]any{"choiceId": "allow", "decision": "approvedForSession", "scope": "session"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.frames <- museRead{frame: museFrame{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: json.RawMessage(`{"status":"accepted","terminal":true}`)}}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	frame := museFrame{JSONRPC: "2.0", Method: "approval/requested", Params: params}
	if err := c.event(ctx, frame); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	frame.Method = "approval/updated"
	if err := c.event(ctx, frame); err != nil {
		t.Fatalf("duplicate terminal update was decided again: %v", err)
	}
	if got := strings.Count(stdin.String(), `"method":"approval/decide"`); got != 1 {
		t.Fatalf("approval/decide calls = %d, want 1; wire=%s", got, stdin.String())
	}
}

func TestMuseDaemonOwnsNetworkSandboxPolicy(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	backend, err := New("muse", Config{ExecutablePath: self, Env: map[string]string{"MUSE_MSP_TEST_HELPER": "network-policy"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := backend.Execute(t.Context(), "hello\nworld", ExecOptions{
		Cwd:        t.TempDir(),
		CustomArgs: []string{"--sandbox-network", "restricted", "--extra"},
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range s.Messages {
	}
	if r := <-s.Result; r.Status != "completed" {
		t.Fatalf("result=%+v", r)
	}
}

func TestMuseModelDiscovery(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSE_MSP_TEST_HELPER", "models")
	catalog, err := ListModels(t.Context(), "muse", NewCommand(self, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "muse-test" || catalog.Models[0].Provider != "meta" || !catalog.Models[0].Default {
		t.Fatalf("catalog=%+v", catalog)
	}
	if !IsKnownThinkingValue("muse", "ultra") || IsKnownThinkingValue("muse", "off") {
		t.Fatal("incorrect MSP effort vocabulary")
	}
}

func TestMuseAllowsEmptyMCPConfig(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	backend, err := New("muse", Config{ExecutablePath: self, Env: map[string]string{"MUSE_MSP_TEST_HELPER": "success"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{" {} ", "null", `{"mcpServers":{}}`} {
		s, err := backend.Execute(t.Context(), "hello\nworld", ExecOptions{Cwd: t.TempDir(), McpConfig: json.RawMessage(raw), Timeout: time.Second})
		if err != nil {
			t.Fatalf("empty config %s: %v", raw, err)
		}
		for range s.Messages {
		}
		if r := <-s.Result; r.Status != "completed" {
			t.Fatalf("result=%+v", r)
		}
	}
	_, err = backend.Execute(t.Context(), "hello", ExecOptions{McpConfig: json.RawMessage(`{"mcpServers":{"test":{"command":"fake"}}}`)})
	if err == nil {
		t.Fatal("managed MCP was silently ignored")
	}
}
