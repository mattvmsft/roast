package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func init() {
	if os.Getenv("ROAST_TEST_COPILOT_PROCESS") != "1" {
		return
	}
	if mode := os.Getenv("ROAST_TEST_COPILOT_PIPE_MODE"); mode != "" {
		if mode == "child" {
			if err := os.WriteFile(os.Getenv("ROAST_TEST_COPILOT_CHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				os.Exit(1)
			}
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
		self, err := os.Executable()
		if err != nil {
			os.Exit(1)
		}
		child := exec.Command(self)
		child.Dir = os.TempDir()
		child.Env = append(os.Environ(), "ROAST_TEST_COPILOT_PIPE_MODE=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(1)
		}
		_ = child.Process.Release()
		if mode == "cancel" {
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
	}
	switch os.Getenv("ROAST_TEST_COPILOT_OUTPUT") {
	case "stdout":
		fmt.Fprint(os.Stdout, strings.Repeat("x", maxCapturedResponseSize+1))
		os.Exit(0)
	case "stderr":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 64<<10))
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--version") {
		fmt.Println("GitHub Copilot CLI test")
		os.Exit(0)
	}
	if detail := os.Getenv("ROAST_TEST_COPILOT_ERROR"); detail != "" {
		fmt.Fprintln(os.Stderr, detail)
		os.Exit(1)
	}
	if os.Getenv("ROAST_TEST_COPILOT_WAIT") == "1" {
		time.Sleep(10 * time.Second)
	}
	input, err := io.ReadAll(os.Stdin)
	entries, dirErr := os.ReadDir(".")
	if err != nil || dirErr != nil || len(entries) != 0 ||
		!strings.Contains(string(input), "ROAST REQUEST:") ||
		slices.Contains(os.Args, "-p") ||
		slices.Contains(os.Args, "--prompt") ||
		!slices.Contains(os.Args, "--available-tools=__roast_inference_only__") ||
		!slices.Contains(os.Args, "--no-custom-instructions") {
		fmt.Fprintln(os.Stderr, "unsafe or incomplete Copilot invocation")
		os.Exit(1)
	}
	if output, ok := os.LookupEnv("ROAST_TEST_COPILOT_EVENTS"); ok {
		fmt.Fprint(os.Stdout, output)
		if os.Getenv("ROAST_TEST_COPILOT_EVENTS_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "CLI event processing failed")
			os.Exit(1)
		}
		os.Exit(0)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type": "assistant.message",
		"data": map[string]any{"content": `{"tool":"probe","arguments":{"ok":true}}`, "toolRequests": []any{}},
	})
	fmt.Println(`{"type":"assistant.turn_end","data":{}}`)
	os.Exit(0)
}

func TestCopilotArgs(t *testing.T) {
	for _, model := range []string{"", "auto", "my-model"} {
		args := copilotArgs(model)
		if slices.Contains(args, "-p") || slices.Contains(args, "--prompt") {
			t.Fatal("explicit prompt flags discard the piped Roast request")
		}
		index := slices.Index(args, "--model")
		if model == "" {
			if index != -1 {
				t.Fatal("default model must omit --model")
			}
		} else if index == -1 || args[index+1] != model {
			t.Fatalf("model not forwarded exactly: %v", args)
		}
		for _, flag := range []string{"--available-tools=__roast_inference_only__", "--no-custom-instructions", "--disable-builtin-mcps", "--silent"} {
			if !slices.Contains(args, flag) {
				t.Fatalf("missing %s", flag)
			}

		}
	}
}

func TestCopilotJSONLOutput(t *testing.T) {
	content := `{"tool":"probe","arguments":{"text":"` + strings.Repeat("long unwrapped text ", 50) + `\nnext line"}}`
	event, err := json.Marshal(map[string]any{
		"type": "assistant.message",
		"data": map[string]any{"content": content, "phase": "final_answer", "toolRequests": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	output := append(event, []byte("\n"+`{"type":"assistant.turn_end","data":{}}`)...)
	body, err := parseCopilotOutput(output)
	if err != nil || string(body) != content {
		t.Fatalf("unwrapped content changed: %q, %v", body, err)
	}
	for _, invalid := range []string{
		"", "not JSONL", string(event),
		`{"type":"assistant.turn_end","data":{}}`,
		`{"type":"session.error","data":{"message":"error"}}`,
		`{"type":"assistant.message","data":{"content":"{}","toolRequests":[{"name":"write"}]}}`,
	} {
		if _, err := parseCopilotOutput([]byte(invalid)); !errors.Is(err, ErrEndpoint) {
			t.Fatalf("invalid JSONL %q did not fail: %v", invalid, err)
		}
	}
}

func copilotProcessClient(t *testing.T) Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROAST_COPILOT_COMMAND", executable)
	t.Setenv("ROAST_TEST_COPILOT_PROCESS", "1")
	client, err := New(t.Context(), ClientConfig{Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCopilotJSONLTurnIsolation(t *testing.T) {
	first := `{"type":"assistant.turn_start"}
{"type":"assistant.message","data":{"content":"first"}}
{"type":"assistant.turn_end"}
{"type":"assistant.turn_start"}
`
	for _, later := range []string{
		`{"type":"assistant.turn_end"}`,
		`{"type":"assistant.message","data":{"content":"commentary","phase":"commentary"}}
{"type":"assistant.turn_end"}`,
	} {
		if body, err := parseCopilotOutput([]byte(first + later)); !errors.Is(err, ErrEndpoint) || body != nil {
			t.Fatalf("stale turn accepted: body=%q err=%v", body, err)
		}
	}
	last := `{"type":"assistant.message","data":{"content":"last","phase":"final_answer"}}
{"type":"assistant.turn_end"}`
	if body, err := parseCopilotOutput([]byte(first + last)); err != nil || string(body) != "last" {
		t.Fatalf("last turn lost: body=%q err=%v", body, err)
	}
}

func TestCopilotMalformedEventsCapture(t *testing.T) {
	for _, failed := range []string{"0", "1"} {
		t.Run("failed="+failed, func(t *testing.T) {
			t.Setenv("ROAST_TEST_COPILOT_EVENTS_FAIL", failed)
			for _, output := range []string{
				"not JSONL",
				`{"type":"assistant.message","data":`,
				`{"type":"assistant.message","data":{"content":"unfinished"}}`,
				`{"type":"assistant.turn_end","data":{}}`,
			} {
				t.Run(output, func(t *testing.T) {
					copilotProcessClient(t)
					t.Setenv("ROAST_TEST_COPILOT_EVENTS", output)
					var captured []RawResponse
					client, err := New(t.Context(), ClientConfig{
						Protocol: "copilot", Role: "finder", Model: "test-model",
						MaxOutputTokens: 100, MaxAttempts: 1,
						CaptureResponse: func(response RawResponse) error {
							captured = append(captured, response)
							return nil
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					var result struct{}
					if err := client.CompleteStructured(t.Context(), "", "Return probe.", "probe", &result); !errors.Is(err, ErrEndpoint) {
						t.Fatalf("error = %v", err)
					}
					if len(captured) != 1 {
						t.Fatalf("captures=%d, want 1", len(captured))
					}
					got := captured[0]
					if string(got.Body) != output || got.ContentType != "text/plain" ||
						got.Role != "finder" || got.Model != "test-model" || got.Protocol != "copilot" {
						t.Fatalf("capture mismatch: %+v", got)
					}
				})
			}
		})
	}
}

func TestCopilotMalformedEventsCaptureFailureIsPermanent(t *testing.T) {
	for _, failed := range []string{"0", "1"} {
		t.Run("failed="+failed, func(t *testing.T) {
			t.Setenv("ROAST_TEST_COPILOT_EVENTS_FAIL", failed)
			copilotProcessClient(t)
			t.Setenv("ROAST_TEST_COPILOT_EVENTS", "not JSONL")
			captureErr := errors.New("disk full")
			calls := 0
			client, err := New(t.Context(), ClientConfig{
				Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: 3,
				CaptureResponse: func(RawResponse) error {
					calls++
					return captureErr
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var result struct{}
			err = client.CompleteStructured(t.Context(), "", "Return probe.", "probe", &result)
			var wrapped *captureError
			if !errors.Is(err, captureErr) || !errors.As(err, &wrapped) || calls != 1 {
				t.Fatalf("capture failure retried or lost: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestCopilotProcessStructured(t *testing.T) {
	client := copilotProcessClient(t)
	var output struct {
		OK bool `json:"ok"`
	}
	if err := client.CompleteStructured(t.Context(), "Return the probe.", "Return ok=true.", "probe", &output); err != nil {
		t.Fatal(err)
	}
	if !output.OK || client.Usage().Calls != 1 || client.Usage().UncachedInputTokens <= 0 || client.Usage().OutputTokens <= 0 {
		t.Fatalf("output=%+v usage=%+v", output, client.Usage())
	}
}

func TestCopilotProcessErrors(t *testing.T) {
	for _, test := range []struct {
		detail string
		want   error
	}{
		{"authentication required", ErrAuth},
		{"unknown model", ErrEndpoint},
	} {
		t.Run(test.detail, func(t *testing.T) {
			client := copilotProcessClient(t)
			t.Setenv("ROAST_TEST_COPILOT_ERROR", test.detail)
			var output struct{}
			err := client.CompleteStructured(t.Context(), "", "Return the probe.", "probe", &output)
			if !errors.Is(err, test.want) || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCopilotProcessCancellation(t *testing.T) {
	client := copilotProcessClient(t)
	t.Setenv("ROAST_TEST_COPILOT_WAIT", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var output struct{}
	err := client.CompleteStructured(ctx, "", "Return the probe.", "probe", &output)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func copilotInheritedPipes(t *testing.T, mode string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("ROAST_TEST_COPILOT_PROCESS", "1")
	t.Setenv("ROAST_TEST_COPILOT_PIPE_MODE", mode)
	t.Setenv("ROAST_TEST_COPILOT_CHILD_PID", pidPath)
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			t.Errorf("read descendant PID: %v", err)
			return
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			t.Errorf("parse descendant PID: %v", err)
			return
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Errorf("find descendant: %v", err)
			return
		}
		if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill descendant: %v", err)
		}
		_, _ = process.Wait()
	})
	return self
}

func TestCopilotCancellationBoundsInheritedPipeWait(t *testing.T) {
	self := copilotInheritedPipes(t, "cancel")
	model := &copilotModel{executable: self, cfg: ClientConfig{MaxOutputTokens: 100}}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := model.execute(ctx, "ROAST REQUEST:")
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancellation did not bound inherited-pipe wait")
	}
}

func TestCopilotExitBoundsInheritedPipeWait(t *testing.T) {
	self := copilotInheritedPipes(t, "exit")
	model := &copilotModel{executable: self, cfg: ClientConfig{MaxOutputTokens: 100}}
	result := make(chan error, 1)
	go func() {
		_, err := model.execute(t.Context(), "ROAST REQUEST:")
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, exec.ErrWaitDelay) || !errors.Is(err, ErrEndpoint) {
			t.Fatalf("error = %v, want explicit pipe-drain failure", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("process exit did not bound inherited-pipe wait")
	}
}

func TestCopilotConverseUsesRoastTools(t *testing.T) {
	calls, reads := 0, 0
	model := &copilotModel{cfg: ClientConfig{MaxOutputTokens: 100}}
	model.run = func(_ context.Context, prompt string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"tool":"read","arguments":{}}`), nil
		}
		if !strings.Contains(prompt, "bounded read result") {
			t.Fatal("Roast tool result not sent to next model turn")
		}
		return []byte(`{"tool":"submit","arguments":{"ok":true}}`), nil
	}
	client := &client{cfg: ClientConfig{Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: 1}, model: model}
	var output struct {
		OK bool `json:"ok"`
	}
	err := client.Converse(t.Context(), "Review the code.", "Use read, then submit.", ConverseOpts{
		Tools: []Tool{
			{Name: "read", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
				reads++
				return "bounded read result", nil
			}},
			{Name: "submit", Schema: reflectSchema(&output)},
		},
		FinalTool: "submit", MaxTurns: 3,
	}, &output)
	if err != nil || !output.OK || reads != 1 || calls != 2 {
		t.Fatalf("err=%v output=%+v reads=%d calls=%d", err, output, reads, calls)
	}
}

func TestCopilotRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{"", "not JSON", "```json\n{}\n```", `{}`, `{"tool":"unknown","arguments":{}}`, `{"tool":"probe","arguments":null}`} {
		t.Run(body, func(t *testing.T) {
			model := &copilotModel{cfg: ClientConfig{MaxOutputTokens: 100}, run: func(context.Context, string) ([]byte, error) {
				return []byte(body), nil
			}}
			stream, err := model.Stream(t.Context(), fantasy.Call{Tools: []fantasy.Tool{
				fantasy.FunctionTool{Name: "probe", InputSchema: map[string]any{"type": "object"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			for part := range stream {
				if part.Type == fantasy.StreamPartTypeFinish {
					continue
				}
				if part.Type != fantasy.StreamPartTypeToolCall || part.ToolCallName != nudgeToolName || !strings.Contains(part.ToolCallInput, "invalid JSON tool envelope") {
					t.Fatalf("unexpected stream part: %+v", part)
				}
			}
		})
	}
}

func TestCopilotCaptureFailure(t *testing.T) {
	captureErr := errors.New("disk full")
	model := &copilotModel{
		cfg: ClientConfig{MaxOutputTokens: 100, CaptureResponse: func(RawResponse) error { return captureErr }},
		run: func(context.Context, string) ([]byte, error) {
			return []byte(`{"tool":"probe","arguments":{}}`), nil
		},
	}
	client := &client{cfg: ClientConfig{Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: 2}, model: model}
	var output struct{}
	if err := client.CompleteStructured(t.Context(), "", "Return the probe.", "probe", &output); !errors.Is(err, captureErr) {
		t.Fatalf("error = %v", err)
	}
}

func TestCopilotRetriesInvalidStructuredArguments(t *testing.T) {
	calls := 0
	model := &copilotModel{
		cfg: ClientConfig{MaxOutputTokens: 100},
		run: func(_ context.Context, prompt string) ([]byte, error) {
			calls++
			if calls == 1 {
				return []byte(`{"tool":"probe","arguments":{"ok":"not a boolean"}}`), nil
			}
			if !strings.Contains(prompt, "previous response was not valid JSON matching the schema") {
				t.Fatal("validation feedback was not sent to Copilot")
			}
			return []byte(`{"tool":"probe","arguments":{"ok":true}}`), nil
		},
	}
	client := &client{cfg: ClientConfig{Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: 2}, model: model}
	var output struct {
		OK bool `json:"ok"`
	}
	err := client.CompleteStructured(t.Context(), "", "Return the probe.", "probe", &output)
	if err != nil || !output.OK || calls != 2 {
		t.Fatalf("err=%v output=%+v calls=%d", err, output, calls)
	}
}

func TestCopilotRetriesMalformedEnvelope(t *testing.T) {
	for _, attempts := range []int{1, 2} {
		t.Run(fmt.Sprint(attempts), func(t *testing.T) {
			calls := 0
			model := &copilotModel{
				cfg: ClientConfig{MaxOutputTokens: 100},
				run: func(_ context.Context, prompt string) ([]byte, error) {
					calls++
					if calls == 1 {
						return []byte("not JSON"), nil
					}
					if !strings.Contains(prompt, "invalid JSON tool envelope") {
						t.Fatal("envelope validation feedback was not sent")
					}
					return []byte(`{"tool":"probe","arguments":{"ok":true}}`), nil
				},
			}
			client := &client{cfg: ClientConfig{Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: attempts}, model: model}
			var output struct {
				OK bool `json:"ok"`
			}
			err := client.CompleteStructured(t.Context(), "", "Return the probe.", "probe", &output)
			if attempts == 1 {
				if err == nil || !strings.Contains(err.Error(), "invalid JSON tool envelope") || output.OK {
					t.Fatalf("invalid envelope must fail explicitly: %v", err)
				}
			} else if err != nil || !output.OK {
				t.Fatalf("retry failed: %v", err)
			}
			if calls != attempts || client.Usage().Calls != attempts {
				t.Fatalf("calls=%d usage=%+v", calls, client.Usage())
			}
		})
	}
}

func TestCopilotBoundedOutput(t *testing.T) {
	b := &boundedBuffer{limit: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("5")); err == nil || b.Len() != 4 {
		t.Fatalf("limit not enforced: err=%v len=%d", err, b.Len())
	}
}

func TestCopilotBoundedOutputCopy(t *testing.T) {
	b := &boundedBuffer{limit: 4}
	// Hide WriterTo so io.Copy exercises the destination's ReadFrom path.
	input := struct{ io.Reader }{strings.NewReader("12345")}
	if _, err := io.Copy(b, input); err == nil || b.Len() > b.limit {
		t.Fatalf("copy bypassed limit: err=%v len=%d", err, b.Len())
	}
}

func TestCopilotProcessOutputLimits(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			client := copilotProcessClient(t)
			t.Setenv("ROAST_TEST_COPILOT_OUTPUT", stream)
			var output struct{}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := client.CompleteStructured(ctx, "", "Return the probe.", "probe", &output)
			if !errors.Is(err, ErrEndpoint) || !strings.Contains(err.Error(), "Copilot output exceeds") {
				t.Fatalf("output limit not enforced: %v", err)
			}
		})
	}
}

func TestCopilotMissingExecutable(t *testing.T) {
	t.Setenv("ROAST_COPILOT_COMMAND", filepath.Join(t.TempDir(), "missing-copilot.exe"))
	if _, err := New(t.Context(), ClientConfig{Protocol: "copilot", MaxOutputTokens: 100}); !errors.Is(err, ErrEndpoint) {
		t.Fatalf("error = %v", err)
	}
}

func TestCopilotRejectsRelativeCommandOverride(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("bin", 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join("bin", "copilot.exe")
	if err := os.WriteFile(executable, []byte("not executed"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(dir, "bin"))
	t.Setenv("PATHEXT", ".EXE")
	for _, override := range []string{"copilot.exe", executable, "." + string(os.PathSeparator) + executable} {
		t.Run(override, func(t *testing.T) {
			t.Setenv("ROAST_COPILOT_COMMAND", override)
			_, err := New(t.Context(), ClientConfig{Protocol: "copilot", MaxOutputTokens: 100})
			if !errors.Is(err, ErrEndpoint) || !strings.Contains(err.Error(), "must be an absolute executable path") {
				t.Fatalf("override %q: error = %v", override, err)
			}
		})
	}
}
