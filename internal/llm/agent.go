package llm

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"charm.land/fantasy"
	jsonschema "github.com/invopop/jsonschema"
)

const nudgeToolName = "__roast_continue"

type agentRunState struct {
	opts        ConverseOpts
	model       string
	wd          *workDeadlineState
	turn        int
	finalTurn   bool
	cutoff      error
	noticeSent  bool
	inputWarn   bool
	finalDone   bool
	finalErr    error
	structured  bool
	out         any
	active      []string
	toolNames   map[string]bool
	toolSchemas map[string]map[string]any

	// Count all final-phase responses, even ones Fantasy rejects before dispatch.
	submissionAttempts  int
	validationExhausted bool
	budgetOnce          sync.Once
}

func (s *agentRunState) notifyBudget() {
	s.budgetOnce.Do(func() {
		if s.opts.OnBudgetExhausted != nil {
			s.opts.OnBudgetExhausted()
		}
	})
}

func (s *agentRunState) forceFinal(cause error) {
	s.finalTurn = true
	if cause != nil && s.cutoff == nil {
		s.cutoff = cause
	}
}

func (s *agentRunState) notice() string {
	switch {
	case errors.Is(s.cutoff, ErrBudgetExhausted):
		return "TOKEN BUDGET EXHAUSTED — call `" + s.opts.FinalTool + "` NOW with everything you have; partial results beat none."
	case errors.Is(s.cutoff, ErrWorkDeadline):
		return s.wd.wrapUpNotice(s.opts.FinalTool)
	default:
		return "Exploration budget exhausted — call `" + s.opts.FinalTool + "` NOW with everything you have; partial results beat none."
	}
}

func (s *agentRunState) cutoffError(what string, cause error) error {
	sentinel := s.cutoff
	if sentinel == nil {
		sentinel = ErrNoFinalCall
	}
	if cause != nil {
		return fmt.Errorf("%w; %s: %w: %v", sentinel, what, ErrNoFinalCall, cause)
	}
	return fmt.Errorf("%w; %s: %w", sentinel, what, ErrNoFinalCall)
}

func (s *agentRunState) prepareStep(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	s.turn++
	messages := opts.Messages
	ceiling := s.opts.InputByteCeiling
	if ceiling <= 0 {
		ceiling = defaultInputByteCeiling
	}
	if s.turn > 1 && !s.inputWarn && fantasyConversationBytes(messages) > ceiling {
		s.inputWarn = true
		messages = append(messages, fantasy.NewUserMessage(
			"INPUT BUDGET EXHAUSTED. Do not request more files. Call `"+s.opts.FinalTool+"` now with what you have — partial results beat none.",
		))
	}
	if s.wd.check() {
		s.forceFinal(ErrWorkDeadline)
	}
	if s.turn >= normalizedMaxTurns(s.opts.MaxTurns) {
		s.forceFinal(nil)
	}

	prepared := fantasy.PrepareStepResult{Messages: messages, ActiveTools: s.active}
	if s.finalTurn {
		prepared.ActiveTools = []string{s.opts.FinalTool}
		if !s.noticeSent {
			s.noticeSent = true
			notice := s.notice()
			if s.structured {
				notice = "Return the requested structured result via the `" + s.opts.FinalTool + "` tool call. Do not answer in prose."
			}
			prepared.Messages = append(prepared.Messages, fantasy.NewUserMessage(notice))
		}
	}
	return ctx, prepared, nil
}

type managedModel struct {
	client *client
	state  *agentRunState
}

func (m *managedModel) Provider() string { return m.client.model.Provider() }
func (m *managedModel) Model() string    { return m.client.model.Model() }

func (m *managedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("roast agent runtime requires streaming")
}

func (m *managedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// Fantasy's AgentTool API keeps only properties and required. Restore the
	// complete caller-supplied JSON Schema at the model boundary so providers
	// that accept it retain top-level constraints such as additionalProperties.
	// Fantasy's Anthropic adapter currently keeps type, properties, and required
	// but discards other top-level keywords, so this restoration is best-effort
	// for providers that accept the complete schema.
	for i, tool := range call.Tools {
		functionTool, ok := tool.(fantasy.FunctionTool)
		if !ok {
			continue
		}
		if schema := m.state.toolSchemas[functionTool.Name]; schema != nil {
			functionTool.InputSchema = cloneSchema(schema)
			call.Tools[i] = functionTool
		}
	}
	final := onlyFinalTool(call.Tools, m.state.opts.FinalTool)
	if final {
		choice := fantasy.ToolChoiceAuto
		call.ToolChoice = &choice
	}
	return func(yield func(fantasy.StreamPart) bool) {
		toolJSON, _ := json.Marshal(call.Tools)
		release, err := m.client.cfg.Ledger.Reserve(fantasyConversationBytes(call.Prompt)+len(toolJSON)+2048, m.client.cfg.MaxOutputTokens, final)
		if err != nil {
			if final {
				if m.state.cutoff == nil {
					m.state.forceFinal(ErrBudgetExhausted)
					m.state.notifyBudget()
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: m.state.cutoffError("no headroom for the wrap-up call", err)})
				return
			}
			m.state.forceFinal(ErrBudgetExhausted)
			m.state.notifyBudget()
			m.streamFinal(ctx, call, yield)
			return
		}

		err = m.streamCall(ctx, call, release, yield)
		deadlineReached := m.state.wd.check()
		if err == nil || errors.Is(err, errStreamStopped) {
			return
		}
		var captureErr *captureError
		if errors.As(err, &captureErr) {
			err = permanentModelError{err: err}
		} else if !final && deadlineReached && ctx.Err() == nil {
			m.state.forceFinal(ErrWorkDeadline)
			m.streamFinal(ctx, call, yield)
			return
		} else if final && m.state.cutoff != nil {
			err = m.state.cutoffError("wrap-up call failed", normalizeProviderError(ctx, m.client.cfg.Model, err))
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
	}, nil
}

var errStreamStopped = errors.New("stream consumer stopped")

func (m *managedModel) streamFinal(ctx context.Context, call fantasy.Call, yield func(fantasy.StreamPart) bool) {
	stream, err := m.Stream(ctx, m.finalCall(call, m.state.notice()))
	if err != nil {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		return
	}
	for part := range stream {
		if !yield(part) {
			return
		}
	}
}

func (m *managedModel) streamCall(ctx context.Context, call fantasy.Call, release func(Usage), yield func(fantasy.StreamPart) bool) (callErr error) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	callCtx, workCancel := m.state.wd.callCtx(callCtx)
	defer workCancel()
	if m.client.cfg.RequestTimeout > 0 {
		var deadlineCancel context.CancelFunc
		callCtx, deadlineCancel = context.WithTimeout(callCtx, m.client.cfg.RequestTimeout)
		defer deadlineCancel()
	}
	body := &responseBody{}
	callCtx = context.WithValue(callCtx, responseBodyKey{}, body)
	var finish *fantasy.StreamPart
	defer func() {
		usage := Usage{}
		if finish != nil {
			usage = m.client.addUsage(finish.Usage)
		}
		release(usage)
	}()
	defer func() {
		cancel()
		_ = body.close()
		if err := m.client.transport.failure(); err != nil {
			callErr = err
		}
	}()
	stream, err := m.client.model.Stream(callCtx, call)
	if err != nil {
		return err
	}
	var content fantasy.ResponseContent
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeError:
			return part.Error
		case fantasy.StreamPartTypeFinish:
			finish = &part
		case fantasy.StreamPartTypeToolCall:
			content = append(content, fantasy.ToolCallContent{
				ToolCallID: part.ID, ToolName: part.ToolCallName, Input: part.ToolCallInput,
				ProviderExecuted: part.ProviderExecuted, ProviderMetadata: part.ProviderMetadata,
			})
		default:
			if !yield(part) {
				return errStreamStopped
			}
		}
	}
	// Capture must succeed before Fantasy is allowed to dispatch any tool, even
	// if the SDK stopped at a terminal event without reading through EOF.
	_ = body.close()
	if err := m.client.transport.failure(); err != nil {
		return err
	}
	if err := callCtx.Err(); err != nil {
		return err
	}
	if finish == nil {
		return fantasy.NewIncompleteStreamError()
	}
	response := &fantasy.Response{Content: content, FinishReason: finish.FinishReason}
	// Let Fantasy reject incomplete/truncated/filtered turns. Rewriting their
	// finish reason to tool_calls could execute a partial tool invocation.
	if finish.FinishReason == fantasy.FinishReasonToolCalls || finish.FinishReason == fantasy.FinishReasonStop {
		response = m.rewriteResponse(response)
	}
	for _, tool := range response.Content.ToolCalls() {
		if !yield(fantasy.StreamPart{
			Type: fantasy.StreamPartTypeToolCall, ID: tool.ToolCallID,
			ToolCallName: tool.ToolName, ToolCallInput: tool.Input,
			ProviderExecuted: tool.ProviderExecuted, ProviderMetadata: tool.ProviderMetadata,
		}) {
			return errStreamStopped
		}
	}
	finish.FinishReason = response.FinishReason
	if !yield(*finish) {
		return errStreamStopped
	}
	return nil
}

type permanentModelError struct{ err error }

func (e permanentModelError) Error() string { return e.err.Error() }

func (m *managedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("roast agent runtime does not generate objects directly")
}

func (m *managedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("roast agent runtime does not stream objects")
}

func (m *managedModel) finalCall(call fantasy.Call, notice string) fantasy.Call {
	m.state.finalTurn = true
	tools := make([]fantasy.Tool, 0, 1)
	for _, tool := range call.Tools {
		if tool.GetName() == m.state.opts.FinalTool {
			tools = append(tools, tool)
		}
	}
	call.Tools = tools
	choice := fantasy.ToolChoiceAuto
	call.ToolChoice = &choice
	call.Prompt = append(call.Prompt, fantasy.NewUserMessage(notice))
	return call
}

func (m *managedModel) canRetrySubmission() bool {
	// Never reopen exploration or start another wrap-up window after a cutoff.
	// Every reminder still goes through the normal token admission and deadlines.
	if m.state.wd.check() {
		m.state.forceFinal(ErrWorkDeadline)
	}
	return m.state.cutoff == nil && m.state.submissionAttempts < maxAttempts(m.client.cfg)
}

func (m *managedModel) rewriteResponse(resp *fantasy.Response) *fantasy.Response {
	if resp == nil {
		return nil
	}

	var finalCall *fantasy.ToolCallContent
	hasToolCall := false
	content := append(fantasy.ResponseContent(nil), resp.Content...)
	for i, item := range content {
		if item.GetType() != fantasy.ContentTypeToolCall {
			continue
		}
		call, ok := fantasy.AsContentType[fantasy.ToolCallContent](item)
		if !ok {
			continue
		}
		hasToolCall = true
		if call.ToolName == m.state.opts.FinalTool {
			finalCall = &call
			continue
		}
		if !m.state.toolNames[call.ToolName] {
			if m.state.opts.OnTurn != nil {
				m.state.opts.OnTurn(TurnInfo{Turn: m.state.turn, Tool: call.ToolName, Args: decodeArgs(call.Input)})
			}
			content[i] = nudgeCall(call.ToolCallID, "unknown tool: "+call.ToolName)
		}
	}

	if m.state.finalTurn {
		m.state.submissionAttempts++
	}
	if finalCall != nil {
		validationErr := validateFinalArguments(finalCall.Input, m.state.out)
		if validationErr != nil && m.state.finalTurn && m.canRetrySubmission() {
			// This failure is provisional. Do not let it mask a provider error
			// from the follow-up call.
			m.state.finalErr = nil
			feedback := fmt.Sprintf("your previous response was not valid JSON matching the schema: %v. Call `%s` again with valid arguments.", validationErr, m.state.opts.FinalTool)
			resp.Content = fantasy.ResponseContent{nudgeCall(finalCall.ToolCallID, feedback)}
			resp.FinishReason = fantasy.FinishReasonToolCalls
			return resp
		}
		m.state.finalErr = validationErr
		if validationErr != nil {
			m.state.validationExhausted = true
		}
		resp.Content = fantasy.ResponseContent{*finalCall}
		resp.FinishReason = fantasy.FinishReasonToolCalls
		return resp
	}

	if m.state.finalTurn {
		// Auto tool selection can produce prose or hallucinated tool calls. Only
		// a validated final submission may finish successfully; never dispatch
		// exploration tools, even ones Fantasy still knows how to execute.
		resp.Content = nil
		resp.FinishReason = fantasy.FinishReasonStop
		if m.canRetrySubmission() {
			resp.Content = fantasy.ResponseContent{nudgeCall(fmt.Sprintf("roast-final-%d", m.state.submissionAttempts),
				"Exploration is finished. Return the result via the `"+m.state.opts.FinalTool+"` tool call, not prose or another tool.")}
			resp.FinishReason = fantasy.FinishReasonToolCalls
		}
		return resp
	}

	resp.Content = content
	if hasToolCall {
		resp.FinishReason = fantasy.FinishReasonToolCalls
		return resp
	}
	if !m.state.finalTurn {
		resp.Content = append(resp.Content, nudgeCall(fmt.Sprintf("roast-nudge-%d", m.state.turn),
			"Results must be delivered via the `"+m.state.opts.FinalTool+"` tool call. If you have enough context call it now; otherwise call an exploration tool."))
		resp.FinishReason = fantasy.FinishReasonToolCalls
	}
	return resp
}

func (c *client) CompleteStructured(ctx context.Context, system, user string, schemaName string, out any) error {
	return runAgent(ctx, c, system, user, ConverseOpts{
		Tools:     []Tool{{Name: schemaName, Description: "Return the requested structured result.", Schema: reflectSchema(out)}},
		FinalTool: schemaName,
		MaxTurns:  1,
	}, out, true)
}

func (c *client) Converse(ctx context.Context, system, user string, opts ConverseOpts, out any) error {
	return runAgent(ctx, c, system, user, opts, out, false)
}

func runAgent(ctx context.Context, c *client, system, user string, opts ConverseOpts, out any, structured bool) error {
	if findTool(opts.Tools, opts.FinalTool) == nil {
		return fmt.Errorf("final tool %q is not configured", opts.FinalTool)
	}
	state := &agentRunState{
		opts: opts, model: c.cfg.Model, wd: newWorkDeadlineState(opts), structured: structured, out: out,
		toolNames: make(map[string]bool, len(opts.Tools)+1), toolSchemas: make(map[string]map[string]any, len(opts.Tools)),
	}
	if structured {
		state.finalTurn = true
	}

	tools := make([]fantasy.AgentTool, 0, len(opts.Tools)+1)
	for i := range opts.Tools {
		info, schema, err := fantasyToolInfo(opts.Tools[i])
		if err != nil {
			return fmt.Errorf("tool %s schema: %w", opts.Tools[i].Name, err)
		}
		state.toolSchemas[opts.Tools[i].Name] = schema
		state.toolNames[opts.Tools[i].Name] = true
		state.active = append(state.active, opts.Tools[i].Name)
		if opts.Tools[i].Name == opts.FinalTool {
			tools = append(tools, finalFantasyTool(info, state))
			continue
		}
		tools = append(tools, explorationFantasyTool(info, opts.Tools[i], state))
	}
	state.toolNames[nudgeToolName] = true
	tools = append(tools, nudgeFantasyTool())

	managed := &managedModel{client: c, state: state}
	agent := fantasy.NewAgent(managed,
		fantasy.WithSystemPrompt(system),
		fantasy.WithMaxOutputTokens(int64(c.cfg.MaxOutputTokens)),
		fantasy.WithTools(tools...),
		fantasy.WithMaxRetries(maxAttempts(c.cfg)-1),
		fantasy.WithPrepareStep(state.prepareStep),
		fantasy.WithRepairToolCall(func(context.Context, fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
			return nil, errors.New("tool-call repair disabled")
		}),
		fantasy.WithStopConditions(func([]fantasy.StepResult) bool {
			return state.finalDone || state.validationExhausted ||
				(state.finalTurn && state.submissionAttempts >= maxAttempts(c.cfg))
		}),
	)

	ctx = context.WithValue(ctx, sessionContextKey{}, "roast-"+rand.Text())
	_, runErr := agent.Stream(ctx, fantasy.AgentStreamCall{Prompt: user})
	var permanent permanentModelError
	if errors.As(runErr, &permanent) {
		// permanentModelError is intentionally opaque while Fantasy classifies
		// retries. Restore its cause once that layer has finished.
		runErr = permanent.err
	}
	if state.finalDone {
		return nil
	}
	if state.finalErr != nil {
		err := fmt.Errorf("%s: %s arguments did not satisfy the schema (%v); if truncated, raise max_output_tokens", state.model, opts.FinalTool, state.finalErr)
		if state.cutoff != nil {
			return state.cutoffError("wrap-up submission was invalid", err)
		}
		return err
	}
	if runErr != nil {
		if state.cutoff != nil {
			return state.cutoffError("wrap-up turn failed", runErr)
		}
		return normalizeProviderError(ctx, c.cfg.Model, runErr)
	}
	if state.cutoff != nil {
		return state.cutoffError("wrap-up turn produced no submission", nil)
	}
	return ErrNoFinalCall
}

type roastFantasyTool struct {
	info fantasy.ToolInfo
	run  func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error)
	opts fantasy.ProviderOptions
}

func (t *roastFantasyTool) Info() fantasy.ToolInfo { return t.info }
func (t *roastFantasyTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.run(ctx, call)
}
func (t *roastFantasyTool) ProviderOptions() fantasy.ProviderOptions        { return t.opts }
func (t *roastFantasyTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.opts = opts }

func explorationFantasyTool(info fantasy.ToolInfo, definition Tool, state *agentRunState) fantasy.AgentTool {
	return &roastFantasyTool{info: info, run: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		args := decodeArgs(call.Input)
		var result any
		var err error
		if definition.Run == nil {
			err = errors.New("tool has no implementation")
		} else {
			result, err = state.wd.runTool(ctx, &definition, args)
		}
		if err != nil {
			result = map[string]any{"error": err.Error()}
		}
		if state.opts.OnTurn != nil {
			state.opts.OnTurn(TurnInfo{Turn: state.turn, Tool: definition.Name, Args: args})
		}
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			data, _ = json.Marshal(map[string]any{"error": marshalErr.Error()})
		}
		return fantasy.NewTextResponse(string(data)), nil
	}}
}

func finalFantasyTool(info fantasy.ToolInfo, state *agentRunState) fantasy.AgentTool {
	return &roastFantasyTool{info: info, run: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		// Fantasy may repair malformed JSON locally. A final submission that
		// Roast already rejected must not become successful after that repair.
		if state.finalErr != nil {
			resp := fantasy.NewTextErrorResponse(state.finalErr.Error())
			resp.StopTurn = true
			return resp, nil
		}
		resp := fantasy.NewTextResponse(call.Input)
		resp.StopTurn = true
		if err := json.Unmarshal([]byte(call.Input), state.out); err != nil {
			state.finalErr = err
			resp = fantasy.NewTextErrorResponse(err.Error())
			resp.StopTurn = true
			return resp, nil
		}
		state.finalErr = nil
		state.finalDone = true
		return resp, nil
	}}
}

func nudgeFantasyTool() fantasy.AgentTool {
	return &roastFantasyTool{
		info: fantasy.ToolInfo{
			Name: nudgeToolName, Description: "Continue the Roast tool loop after a free-form response.",
			Parameters: map[string]any{"message": map[string]any{"type": "string"}}, Required: []string{"message"},
		},
		run: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var payload struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(call.Input), &payload)
			return fantasy.NewTextResponse(payload.Message), nil
		},
	}
}

func nudgeCall(id, message string) fantasy.ToolCallContent {
	data, _ := json.Marshal(map[string]string{"message": message})
	return fantasy.ToolCallContent{ToolCallID: id, ToolName: nudgeToolName, Input: string(data)}
}

func decodeArgs(arguments string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return map[string]any{"error": fmt.Sprintf("args parse error: %v", err)}
	}
	return args
}

func fantasyToolInfo(t Tool) (fantasy.ToolInfo, map[string]any, error) {
	data, err := json.Marshal(t.Schema)
	if err != nil {
		return fantasy.ToolInfo{}, nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return fantasy.ToolInfo{}, nil, err
	}
	parameters, _ := schema["properties"].(map[string]any)
	if parameters == nil {
		parameters = map[string]any{}
	}
	var required []string
	if raw, ok := schema["required"].([]any); ok {
		for _, item := range raw {
			if name, ok := item.(string); ok {
				required = append(required, name)
			}
		}
	}
	return fantasy.ToolInfo{Name: t.Name, Description: t.Description, Parameters: parameters, Required: required}, schema, nil
}

func cloneSchema(schema map[string]any) map[string]any {
	data, _ := json.Marshal(schema)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	return clone
}

func reflectSchema(v any) map[string]any {
	r := &jsonschema.Reflector{Anonymous: true, AllowAdditionalProperties: false, DoNotReference: true}
	s := r.Reflect(v)
	s.Version = ""
	data, _ := json.Marshal(s)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func validateFinalArguments(arguments string, out any) error {
	data := json.RawMessage(arguments)
	if !json.Valid(data) {
		return errors.New("invalid JSON")
	}
	t := reflect.TypeOf(out)
	if t == nil || t.Kind() != reflect.Pointer {
		return errors.New("output must be a pointer")
	}
	scratch := reflect.New(t.Elem()).Interface()
	return json.Unmarshal(data, scratch)
}

func normalizedMaxTurns(turns int) int {
	if turns <= 0 {
		return 8
	}
	return turns
}

func findTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func onlyFinalTool(tools []fantasy.Tool, final string) bool {
	return len(tools) == 1 && tools[0] != nil && tools[0].GetName() == final
}

func fantasyConversationBytes(messages fantasy.Prompt) int {
	data, _ := json.Marshal(messages)
	return len(data)
}
