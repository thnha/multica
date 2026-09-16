package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// MSP is Muse's native session protocol, not ACP or Codex app-server.
// Wire shapes follow `muse schema generate-ts` (stable v1, Muse Code 1.1.1).
type museBackend struct{ cfg Config }

var museBlockedArgs = map[string]blockedArgMode{
	"--no-session-log":   blockedStandalone,
	"--model":            blockedWithValue,
	"--workspace":        blockedWithValue,
	"--approval-mode":    blockedWithValue,
	"--reasoning-effort": blockedWithValue,
	"--sandbox-network":  blockedWithValue,
	"--json":             blockedStandalone,
}

type museRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Kind string `json:"kind"`
	} `json:"data"`
}

func (e *museRPCError) Error() string {
	return fmt.Sprintf("muse MSP: %s (%s)", e.Message, e.Data.Kind)
}

type museFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *museRPCError   `json:"error,omitempty"`
}
type museRead struct {
	frame museFrame
	err   error
}

type museClient struct {
	stdin               io.WriteCloser
	frames              chan museRead
	responses           map[string]museFrame
	decidedRequirements map[string]struct{}
	resolvedApprovalIDs map[string]struct{}
	writeMu             sync.Mutex
	nextID              int
	onEvent             func(museFrame) error
}

func (c *museClient) write(ctx context.Context, v any) error {
	done := make(chan error, 1)
	go func() { c.writeMu.Lock(); defer c.writeMu.Unlock(); done <- json.NewEncoder(c.stdin).Encode(v) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *museClient) read(ctx context.Context) (museFrame, error) {
	select {
	case r := <-c.frames:
		return r.frame, r.err
	case <-ctx.Done():
		return museFrame{}, ctx.Err()
	}
}

func (c *museClient) call(ctx context.Context, method string, params any, out any) error {
	c.nextID++
	id := c.nextID
	responseID := fmt.Sprint(id)
	if err := c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		f, ok := c.responses[responseID]
		if ok {
			delete(c.responses, responseID)
		} else {
			var err error
			f, err = c.read(ctx)
			if err != nil {
				return err
			}
		}
		if f.Method != "" {
			if err := c.event(ctx, f); err != nil {
				return err
			}
			continue
		}
		if string(f.ID) != responseID {
			// Approval notifications can arrive before the response to the RPC
			// currently on the stack. Deciding that approval makes a nested RPC;
			// retain the outer response instead of dropping it as an unknown ID.
			if len(f.ID) > 0 {
				if c.responses == nil {
					c.responses = make(map[string]museFrame)
				}
				c.responses[string(f.ID)] = f
			}
			continue
		}
		if f.Error != nil {
			return f.Error
		}
		if len(f.Result) == 0 {
			return fmt.Errorf("muse MSP %s: missing result", method)
		}
		if out != nil {
			if err := json.Unmarshal(f.Result, out); err != nil {
				return fmt.Errorf("muse MSP %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (c *museClient) event(ctx context.Context, f museFrame) error {
	if len(f.ID) > 0 {
		return fmt.Errorf("muse MSP requires unsupported interaction: %s", f.Method)
	}
	if f.Method == "approval/requested" || f.Method == "approval/updated" {
		var req struct {
			ApprovalID           string `json:"approvalId"`
			SessionID            string `json:"sessionId"`
			CurrentRequirementID struct {
				ApprovalID  string `json:"approvalId"`
				SourceIndex int    `json:"sourceIndex"`
			} `json:"currentRequirementId"`
			AvailableChoices []struct {
				ChoiceID string `json:"choiceId"`
				Decision string `json:"decision"`
				Scope    string `json:"scope"`
			} `json:"availableChoices"`
		}
		if err := json.Unmarshal(f.Params, &req); err != nil || req.ApprovalID == "" || req.SessionID == "" {
			return errors.New("muse MSP approval request has invalid parameters")
		}
		if _, resolved := c.resolvedApprovalIDs[req.ApprovalID]; resolved {
			return nil
		}
		requirementKey := fmt.Sprintf("%s:%d", req.ApprovalID, req.CurrentRequirementID.SourceIndex)
		if _, decided := c.decidedRequirements[requirementKey]; decided {
			return nil
		}
		choiceID := ""
		for _, choice := range req.AvailableChoices {
			if choice.Decision == "approvedForSession" && choice.Scope == "session" {
				choiceID = choice.ChoiceID
				break
			}
		}
		if choiceID == "" {
			for _, choice := range req.AvailableChoices {
				if choice.Decision == "approved" && choice.Scope == "once" {
					choiceID = choice.ChoiceID
					break
				}
			}
		}
		if choiceID == "" {
			return errors.New("muse MSP approval request has no safe automatic choice")
		}
		params := map[string]any{
			"commandId": museCommandID(), "approvalId": req.ApprovalID,
			"sessionId": req.SessionID, "choiceId": choiceID,
			"requirementId": req.CurrentRequirementID,
		}
		if c.decidedRequirements == nil {
			c.decidedRequirements = make(map[string]struct{})
		}
		c.decidedRequirements[requirementKey] = struct{}{}
		var decision struct {
			Terminal bool `json:"terminal"`
		}
		if err := c.call(ctx, "approval/decide", params, &decision); err != nil {
			var rpcErr *museRPCError
			if errors.As(err, &rpcErr) && rpcErr.Data.Kind == "approvalAlreadyResolved" {
				if c.resolvedApprovalIDs == nil {
					c.resolvedApprovalIDs = make(map[string]struct{})
				}
				c.resolvedApprovalIDs[req.ApprovalID] = struct{}{}
				return nil
			}
			delete(c.decidedRequirements, requirementKey)
			return fmt.Errorf("muse MSP approval decision: %w", err)
		}
		if decision.Terminal {
			if c.resolvedApprovalIDs == nil {
				c.resolvedApprovalIDs = make(map[string]struct{})
			}
			c.resolvedApprovalIDs[req.ApprovalID] = struct{}{}
		}
		return nil
	}
	if f.Method == "userInput/requested" {
		return fmt.Errorf("muse MSP requires unsupported interaction: %s", f.Method)
	}
	if c.onEvent != nil {
		return c.onEvent(f)
	}
	return nil
}

func museCommandID() string { return uuid.Must(uuid.NewV7()).String() }

func (b *museBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	path := b.cfg.ExecutablePath
	if path == "" {
		path = "muse"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("muse executable: %w", err)
	}
	if len(opts.McpConfig) > 0 {
		var config map[string]json.RawMessage
		if err := json.Unmarshal(opts.McpConfig, &config); err != nil {
			return nil, fmt.Errorf("invalid Muse MCP configuration: %w", err)
		}
		if raw, ok := config["mcpServers"]; ok {
			var servers map[string]json.RawMessage
			if json.Unmarshal(raw, &servers) == nil && len(servers) == 0 {
				delete(config, "mcpServers")
			}
		}
		if len(config) > 0 {
			return nil, errors.New("muse MSP does not support per-agent MCP configuration")
		}
	}
	workDir := opts.Cwd
	if workDir == "" {
		workDir = "."
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := runContext(ctx, opts.Timeout)
	// Keep the process alive briefly after caller cancellation to deliver MSP
	// turn/interrupt. The cleanup below owns its entire process tree.
	// The task-scoped Multica CLI talks to the daemon API over loopback.
	// Muse's proxy-only default blocks that route, while enabled changes only
	// the network posture and preserves the filesystem sandbox.
	args := []string{"serve", "--trust-workspace", "--sandbox-network", "enabled"}
	args = append(args, filterCustomArgs(append(append([]string{}, opts.ExtraArgs...), opts.CustomArgs...), museBlockedArgs, b.cfg.Logger)...)
	cfg := b.cfg
	cfg.ExecutablePath = resolved
	c, closeHost, err := startMuseHost(cfg, args, workDir, len(prompt))
	if err != nil {
		cancel()
		return nil, err
	}
	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	var terminal atomic.Bool
	go func() {
		start := time.Now()
		state := museTurnState{items: make(map[string]museItem), result: Result{Status: "failed", SessionID: opts.ResumeSessionID}, emit: func(m Message) {
			select {
			case msgCh <- m:
			case <-runCtx.Done():
			}
		}, terminal: &terminal}
		c.onEvent = state.event
		err := b.runMSP(runCtx, c, &state, prompt, workDir, opts)
		if runCtx.Err() != nil && !terminal.Load() {
			state.result.Status = "cancelled"
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				state.result.Status = "timeout"
			}
			if state.turnID != "" {
				grace := opts.TurnInterruptTimeout
				if grace <= 0 {
					grace = 2 * time.Second
				}
				interruptCtx, done := context.WithTimeout(context.Background(), grace)
				// Preserve caller cancellation even if the host reports a terminal
				// while acknowledging the interrupt.
				settled := false
				c.onEvent = func(f museFrame) error {
					var p struct {
						SessionID string `json:"sessionId"`
						TurnID    string `json:"turnId"`
					}
					if f.Method == "turn/completed" && json.Unmarshal(f.Params, &p) == nil && p.SessionID == state.result.SessionID && p.TurnID == state.turnID {
						settled = true
					}
					return nil
				}
				if c.call(interruptCtx, "turn/interrupt", map[string]any{"commandId": museCommandID(), "sessionId": state.result.SessionID, "turnId": state.turnID}, nil) == nil {
					for !settled {
						f, err := c.read(interruptCtx)
						if err != nil {
							break
						}
						if f.Method != "" {
							if c.event(interruptCtx, f) != nil {
								break
							}
						}
					}
				}
				done()
			}
		} else if err != nil && !terminal.Load() {
			state.result.Error = err.Error()
		}
		// EOF normally stops serve. Kill descendants as well, including tools
		// that retain inherited stdout or fail to respond to the interrupt.
		closeHost()
		cancel()
		state.result.DurationMs = time.Since(start).Milliseconds()
		close(msgCh)
		resCh <- state.result
		close(resCh)
	}()
	return &Session{Messages: msgCh, Result: resCh, TerminalObserved: terminal.Load}, nil
}

func (b *museBackend) runMSP(ctx context.Context, c *museClient, s *museTurnState, prompt, workDir string, opts ExecOptions) error {
	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	handshake, done := context.WithTimeout(ctx, timeout)
	defer done()
	if err := c.initialize(handshake, b.cfg.DaemonVersion); err != nil {
		return err
	}
	method := "session/start"
	params := map[string]any{"commandId": museCommandID(), "workspaceRoot": workDir, "approvalMode": "allowAll"}
	if opts.Model != "" {
		params["modelId"] = opts.Model
		if opts.ModelProvider != "" {
			params["providerId"] = opts.ModelProvider
		}
	}
	if opts.ResumeSessionID != "" {
		method = "session/resume"
		params = map[string]any{"commandId": museCommandID(), "sessionId": opts.ResumeSessionID, "excludeItems": true}
	}
	var session struct {
		Session struct {
			ID           string  `json:"sessionId"`
			Model        string  `json:"modelId"`
			Status       string  `json:"status"`
			ActiveTurnID *string `json:"activeTurnId"`
		} `json:"session"`
	}
	if err := c.call(handshake, method, params, &session); err != nil {
		var rpcErr *museRPCError
		if method == "session/resume" && errors.As(err, &rpcErr) {
			s.result.ResumeRejected = rpcErr.Data.Kind == "sessionNotFound" || rpcErr.Data.Kind == "boundaryUnusable"
			s.result.ResumeRejectedTransient = rpcErr.Data.Kind == "sessionInUse"
		}
		return err
	}
	if session.Session.ID == "" {
		return errors.New("muse MSP returned no session ID")
	}
	if opts.ResumeSessionID != "" && session.Session.ID != opts.ResumeSessionID {
		return errors.New("muse MSP resumed a different session")
	}
	if session.Session.Status == "running" || session.Session.ActiveTurnID != nil {
		s.result.ResumeRejectedTransient = opts.ResumeSessionID != ""
		return errors.New("muse session already has an active turn")
	}
	s.result.SessionID = session.Session.ID
	s.model = session.Session.Model
	s.emit(Message{Type: MessageStatus, Status: "running", SessionID: s.result.SessionID})
	if opts.ResumeSessionID != "" {
		if err := c.call(handshake, "session/setApprovalMode", map[string]any{"commandId": museCommandID(), "sessionId": s.result.SessionID, "mode": "allowAll"}, nil); err != nil {
			return err
		}
		if opts.Model != "" {
			model := map[string]any{"modelId": opts.Model}
			if opts.ModelProvider != "" {
				model["providerId"] = opts.ModelProvider
			}
			if err := c.call(handshake, "session/setModel", map[string]any{"commandId": museCommandID(), "sessionId": s.result.SessionID, "model": model}, nil); err != nil {
				return err
			}
			s.model = opts.Model
		}
	}
	if opts.MaxTurns > 0 {
		b.cfg.Logger.Warn("muse MSP does not expose a per-turn model-step limit; ignoring maxTurns")
	}
	s.turnID = museCommandID()
	params = map[string]any{"commandId": s.turnID, "sessionId": s.result.SessionID, "input": []map[string]any{{"type": "text", "text": prompt}}, "ifBusy": "queue"}
	if opts.ThinkingLevel != "" {
		params["reasoningEffort"] = opts.ThinkingLevel
	}
	var ack struct {
		TurnID string `json:"turnId"`
		Status string `json:"status"`
	}
	if err := c.call(handshake, "turn/start", params, &ack); err != nil {
		return err
	}
	if ack.Status != "accepted" || ack.TurnID != s.turnID {
		return errors.New("muse MSP did not start the requested turn")
	}
	for !s.terminal.Load() {
		f, err := c.read(ctx)
		if err != nil {
			return err
		}
		if f.Method != "" {
			if err := c.event(ctx, f); err != nil {
				return err
			}
		}
	}
	return nil
}

type museItem struct {
	ID       string   `json:"itemId"`
	TurnID   string   `json:"turnId"`
	Kind     string   `json:"kind"`
	Revision int      `json:"revision"`
	Status   string   `json:"status"`
	Text     string   `json:"text"`
	Summary  []string `json:"summary"`
	Tool     string   `json:"tool"`
	CallID   string   `json:"callId"`
	Args     string   `json:"args"`
	Output   string   `json:"visibleOutput"`
	Fallback string   `json:"fallbackText"`
}
type museTurnState struct {
	items         map[string]museItem
	turnID, model string
	result        Result
	emit          func(Message)
	terminal      *atomic.Bool
}

func (s *museTurnState) event(f museFrame) error {
	if s.turnID == "" || s.terminal.Load() {
		return nil
	}
	var p struct {
		SessionID string   `json:"sessionId"`
		TurnID    string   `json:"turnId"`
		Item      museItem `json:"item"`
		ItemID    string   `json:"itemId"`
		Delta     string   `json:"delta"`
		Field     string   `json:"field"`
		Terminal  string   `json:"terminal"`
		Error     struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			Input      int64 `json:"inputTokens"`
			Output     int64 `json:"outputTokens"`
			CacheRead  int64 `json:"cacheReadTokens"`
			CacheWrite int64 `json:"cacheWriteTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(f.Params, &p); err != nil {
		return fmt.Errorf("muse MSP invalid %s params", f.Method)
	}
	if p.SessionID != s.result.SessionID {
		return nil
	}
	switch f.Method {
	case "view/gap":
		return errors.New("muse MSP event gap: transcript continuity lost")
	case "item/started", "item/updated", "item/completed":
		i := p.Item
		if i.TurnID != s.turnID || i.ID == "" {
			return nil
		}
		old, seen := s.items[i.ID]
		if seen && i.Revision <= old.Revision {
			return nil
		}
		s.items[i.ID] = i
		switch i.Kind {
		case "agentMessage":
			if strings.HasPrefix(i.Text, old.Text) && len(i.Text) > len(old.Text) {
				s.emit(Message{Type: MessageText, Content: i.Text[len(old.Text):]})
			}
			if f.Method == "item/completed" {
				s.result.Output = i.Text
			}
		case "reasoning":
			if f.Method == "item/completed" && len(i.Summary) > 0 {
				s.emit(Message{Type: MessageThinking, Content: strings.Join(i.Summary, "\n")})
			}
		case "toolCall":
			callID := i.CallID
			if callID == "" {
				callID = i.ID
			}
			if !seen {
				var input map[string]any
				_ = json.Unmarshal([]byte(i.Args), &input)
				s.emit(Message{Type: MessageToolUse, Tool: i.Tool, CallID: callID, Input: input})
			}
			if i.Status != "inProgress" && (!seen || old.Status == "inProgress") {
				s.emit(Message{Type: MessageToolResult, Tool: i.Tool, CallID: callID, Output: i.Output})
			}
		default:
			if i.Fallback != "" {
				s.emit(Message{Type: MessageStatus, Content: i.Fallback})
			}
		}
	case "item/delta":
		i, ok := s.items[p.ItemID]
		if !ok || i.TurnID != s.turnID {
			return nil
		}
		if i.Kind == "agentMessage" && (p.Field == "" || p.Field == "text") {
			i.Text += p.Delta
			s.items[p.ItemID] = i
			s.emit(Message{Type: MessageText, Content: p.Delta})
		}
	case "turn/completed":
		if p.TurnID != s.turnID {
			return nil
		}
		s.result.Status = p.Terminal
		switch p.Terminal {
		case "completed", "cancelled":
		case "failed":
			s.result.Error = p.Error.Message
		default:
			s.result.Status = "failed"
			s.result.Error = "unknown muse turn terminal: " + p.Terminal
		}
		s.result.Usage = map[string]TokenUsage{s.model: {InputTokens: p.Usage.Input, OutputTokens: p.Usage.Output, CacheReadTokens: p.Usage.CacheRead, CacheWriteTokens: p.Usage.CacheWrite}}
		s.terminal.Store(true)
	}
	return nil
}

// startMuseHost owns a native MSP stdio host. The caller must close it on every path.
func startMuseHost(cfg Config, args []string, workDir string, promptBytes int) (*museClient, func(), error) {
	processCtx, stopProcess := context.WithCancel(context.Background())
	cmd := cfg.commandAt(cfg.ExecutablePath).exec(processCtx, args...)
	hideAgentWindow(cmd)
	cmd.Dir = workDir
	cmd.Env = buildEnv(cfg.Env)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stderr = newLogWriter(cfg.Logger, "[muse:stderr] ")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		stopProcess()
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		stopProcess()
		return nil, nil, err
	}
	cfg.logAgentCommandWithPrompt(cmd, newAgentCommandLogArgs(args, trustAgentCommandPositional(0, "serve")), promptBytes)
	if err := startOwnedProcessTree(cmd, cfg.Logger); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		stopProcess()
		return nil, nil, err
	}
	c := &museClient{stdin: stdin, frames: make(chan museRead, 64)}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 16<<20)
		for scanner.Scan() {
			var f museFrame
			err := json.Unmarshal(scanner.Bytes(), &f)
			if err == nil && f.JSONRPC != "2.0" {
				err = errors.New("invalid MSP envelope")
			}
			if err != nil {
				err = errors.New("muse returned a malformed MSP frame")
			}
			select {
			case c.frames <- museRead{f, err}:
			case <-processCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = errors.New("muse MSP stream ended before turn completion")
		}
		select {
		case c.frames <- museRead{err: err}:
		case <-processCtx.Done():
		}
	}()

	return c, func() {
		_ = stdin.Close()
		signalProcessGroup(cmd, syscall.SIGKILL)
		stopProcess()
		_ = stdout.Close()
		<-readerDone
		_ = cmd.Wait()
		releaseProcessGroup(cmd)
	}, nil
}

func (c *museClient) initialize(ctx context.Context, version string) error {
	var init struct {
		Schema struct {
			Version int `json:"version"`
		} `json:"schema"`
	}
	if err := c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "multica", "version": version}, "capabilities": map[string]any{"userInputDialogs": false}}, &init); err != nil {
		return err
	}
	if init.Schema.Version != 1 {
		return fmt.Errorf("unsupported Muse MSP schema version %d", init.Schema.Version)
	}
	if err := c.write(ctx, map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		return err
	}
	return nil
}
