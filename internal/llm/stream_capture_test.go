package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

func TestAnthropicInterruptedStreamRetriesWithoutExecutingPartialTools(t *testing.T) {
	var calls, tools atomic.Int32
	var captured []RawResponse
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		tool, arguments := "read", `{"path":"fixture.go"}`
		if call == 3 {
			tool, arguments = "submit", `{"answer":"done"}`
		}
		fixture := httptest.NewRecorder()
		if err := testutil.WriteInferenceStream(fixture, "anthropic", anthropicResponse(tool, arguments)); err != nil {
			t.Error(err)
			return
		}
		body := fixture.Body.String()
		if call == 1 {
			// Include the complete tool input and stop_reason, but drop the
			// terminal message_stop event. This is not a successful turn.
			body = body[:strings.Index(body, "event: message_stop")]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	ledger := NewLedger(1000000)
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "claude-test", Protocol: "anthropic",
		MaxOutputTokens: 32768, MaxAttempts: 2, Ledger: ledger,
		CaptureResponse: func(response RawResponse) error { captured = append(captured, response); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Answer string }
	err = c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
			tools.Add(1)
			return "contents", nil
		}),
	}, &out)
	if err != nil || calls.Load() != 3 || tools.Load() != 1 || out.Answer != "done" {
		t.Fatalf("error=%v requests=%d tools=%d answer=%q", err, calls.Load(), tools.Load(), out.Answer)
	}
	if len(captured) != 3 || strings.Contains(string(captured[0].Body), "event: message_stop") || !strings.Contains(string(captured[1].Body), "event: message_stop") {
		t.Fatalf("interrupted and complete streams were not preserved: %+v", captured)
	}
	spent, _ := ledger.Committed()
	if c.Usage().Calls != 2 || spent != int(weightedTokens(c.Usage())) || ledger.outstanding != 0 {
		t.Fatalf("usage=%+v spent=%d outstanding=%v", c.Usage(), spent, ledger.outstanding)
	}
}

type trackedResponseBody struct {
	io.Reader
	reads, closes int
}

func (b *trackedResponseBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *trackedResponseBody) Close() error               { b.closes++; return nil }

func TestStreamCaptureDoesNotEagerlyReadAndPreservesPartialBytes(t *testing.T) {
	raw := ": keepalive\r\n\r\nevent: message_start\r\ndata: {}\r\n\r\n"
	body := &trackedResponseBody{Reader: strings.NewReader(raw)}
	var captured []RawResponse
	transport := &captureTransport{
		base: sessionRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, Body: body, ContentLength: -1}, nil
		}),
		cfg: ClientConfig{Protocol: "anthropic", CaptureResponse: func(response RawResponse) error { captured = append(captured, response); return nil }},
	}
	owner := &responseBody{}
	request, _ := http.NewRequestWithContext(context.WithValue(t.Context(), responseBodyKey{}, owner), http.MethodPost, "https://inference.example/v1/messages", nil)
	response, err := transport.RoundTrip(request)
	if err != nil || body.reads != 0 {
		t.Fatalf("RoundTrip error=%v eager reads=%d", err, body.reads)
	}
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || string(captured[0].Body) != raw[:5] || captured[0].ContentType != "text/event-stream" || body.closes != 1 {
		t.Fatalf("captured=%+v closes=%d", captured, body.closes)
	}
}

func TestStreamCaptureEnforcesLimitWithoutContentLength(t *testing.T) {
	called := false
	transport := &captureTransport{cfg: ClientConfig{CaptureResponse: func(RawResponse) error { called = true; return nil }}}
	body := &trackedResponseBody{Reader: strings.NewReader(strings.Repeat("x", maxCapturedResponseSize+1))}
	capture := &streamCaptureBody{body: body, transport: transport}
	_, err := io.Copy(io.Discard, capture)
	var captureErr *captureError
	if !errors.As(err, &captureErr) || !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("error=%v", err)
	}
	if err := capture.Close(); !errors.As(err, &captureErr) || called || body.closes != 1 || capture.data.Len() > maxCapturedResponseSize {
		t.Fatalf("close error=%v callback=%v closes=%d captured bytes=%d", err, called, body.closes, capture.data.Len())
	}
}
