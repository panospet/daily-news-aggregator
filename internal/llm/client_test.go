package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCall_NoAPIKey_ReturnsError(t *testing.T) {
	c := &Client{HTTPClient: http.DefaultClient, CommandCodeAPIKey: ""}
	if _, _, err := c.Call("hello", nil); err == nil {
		t.Error("expected error for empty API key")
	}
}

func TestCall_SuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("missing bearer token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{"message": map[string]string{"content": `{"result": "ok"}`}},
			},
		})
	}))
	defer server.Close()

	c := &Client{HTTPClient: server.Client(), CommandCodeAPIKey: "test-key", Endpoint: server.URL}
	result, err := c.CallWithModel("test prompt", "test-model", 100, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"result": "ok"}` {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestCall_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "internal error"}`))
	}))
	defer server.Close()

	c := &Client{HTTPClient: server.Client(), CommandCodeAPIKey: "test-key", Endpoint: server.URL}
	_, err := c.CallWithModel("test", "model", 100, nil)
	if err == nil {
		t.Error("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should carry the status code, got %v", err)
	}
}

func TestCall_EmptyCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{}})
	}))
	defer server.Close()

	c := &Client{HTTPClient: server.Client(), CommandCodeAPIKey: "test-key", Endpoint: server.URL}
	if _, err := c.CallWithModel("test", "model", 100, nil); err == nil {
		t.Error("expected error for empty candidates")
	}
}

// sseServer streams the given content chunks as OpenAI-style SSE frames.
func sseServer(t *testing.T, chunks []string, assertRequest func(*request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if assertRequest != nil {
			var req request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding request: %v", err)
			}
			assertRequest(&req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range chunks {
			frame := map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{"delta": map[string]string{"content": chunk}},
				},
			}
			body, _ := json.Marshal(frame)
			fmt.Fprintf(w, "data: %s\n\n", body)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestCallStreamContext_AccumulatesDeltasInOrder(t *testing.T) {
	chunks := []string{`{"summary": "Πε`, `ρίληψη με στοιχεία [1]`, ` και άλλα."`, `, "topic": "Θέμα"}`}
	server := sseServer(t, chunks, func(req *request) {
		if !req.Stream {
			t.Error("streaming call must set stream=true")
		}
		if req.Model != "MiniMaxAI/MiniMax-M3" {
			t.Errorf("unexpected model: %s", req.Model)
		}
	})
	defer server.Close()

	c := &Client{HTTPClient: server.Client(), CommandCodeAPIKey: "test-key", Endpoint: server.URL}
	var seen []string
	full, err := c.CallStreamContext(context.Background(), "prompt", nil, func(delta string) error {
		seen = append(seen, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(seen, ""), strings.Join(chunks, ""); got != want {
		t.Errorf("deltas out of order:\n got %q\nwant %q", got, want)
	}
	if full != strings.Join(chunks, "") {
		t.Errorf("accumulated text mismatch: %q", full)
	}
	if len(seen) != len(chunks) {
		t.Errorf("expected %d deltas, got %d", len(chunks), len(seen))
	}
}

func TestCallStreamContext_ReaderErrorAbortsGeneration(t *testing.T) {
	server := sseServer(t, []string{"one", "two", "three"}, nil)
	defer server.Close()

	c := &Client{HTTPClient: server.Client(), CommandCodeAPIKey: "test-key", Endpoint: server.URL}
	stop := errors.New("reader gone")
	calls := 0
	if _, err := c.CallStreamContext(context.Background(), "prompt", nil, func(string) error {
		calls++
		return stop
	}); !errors.Is(err, stop) {
		t.Fatalf("expected the reader's error to surface, got %v", err)
	}
	if calls != 1 {
		t.Errorf("generation should stop at the first failed delta, got %d calls", calls)
	}
}

func TestCallStreamContext_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer server.Close()

	c := &Client{HTTPClient: server.Client(), CommandCodeAPIKey: "test-key", Endpoint: server.URL}
	if _, err := c.CallStreamContext(context.Background(), "prompt", nil, nil); err == nil {
		t.Error("expected an error for a non-200 streaming response")
	}
}

func TestCallStreamContext_EmptyStreamIsAnError(t *testing.T) {
	server := sseServer(t, nil, nil)
	defer server.Close()

	c := &Client{HTTPClient: server.Client(), CommandCodeAPIKey: "test-key", Endpoint: server.URL}
	if _, err := c.CallStreamContext(context.Background(), "prompt", nil, nil); err == nil {
		t.Error("a stream with no content should be an error, so the caller can fall back")
	}
}

func TestCallStreamContext_NoAPIKey(t *testing.T) {
	c := &Client{HTTPClient: http.DefaultClient, CommandCodeAPIKey: ""}
	if _, err := c.CallStreamContext(context.Background(), "prompt", nil, nil); err == nil {
		t.Error("expected error for empty API key")
	}
}
