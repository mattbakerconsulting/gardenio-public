package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type openaiEmbedRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

// openaiEmbedResponse matches the shape produced by OpenAI, LiteLLM, and
// any OpenAI-compatible proxy.
func openaiResponse(vectors [][]float32) map[string]any {
	data := make([]map[string]any, len(vectors))
	for i, v := range vectors {
		data[i] = map[string]any{"embedding": v, "index": i}
	}
	return map[string]any{"data": data, "model": "test", "usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1}}
}

func TestOpenAICompatEmbedder_Defaults(t *testing.T) {
	e := NewOpenAICompatEmbedder("https://example.com", "bedrock-titan-embed-v2", "")
	if got, want := e.ModelName(), "bedrock-titan-embed-v2"; got != want {
		t.Errorf("ModelName() = %q, want %q", got, want)
	}
	if got, want := e.Dimensions(), defaultOpenAIDims; got != want {
		t.Errorf("Dimensions() = %d, want %d", got, want)
	}
}

func TestOpenAICompatEmbedder_WithDimensions(t *testing.T) {
	e := NewOpenAICompatEmbedder("", "text-embedding-3-large", "").WithDimensions(3072)
	if got, want := e.Dimensions(), 3072; got != want {
		t.Errorf("Dimensions() = %d, want %d", got, want)
	}
}

func TestOpenAICompatEmbedder_Embed(t *testing.T) {
	var gotReq openaiEmbedRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %s, want /embeddings", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(openaiResponse([][]float32{{0.1, 0.2, 0.3}}))
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "bedrock-titan-embed-v2", "secret-token")
	vec, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	want := []float32{0.1, 0.2, 0.3}
	if len(vec) != len(want) {
		t.Fatalf("len(vec) = %d, want %d", len(vec), len(want))
	}
	for i, v := range want {
		if vec[i] != v {
			t.Errorf("vec[%d] = %v, want %v", i, vec[i], v)
		}
	}
	if gotReq.Model != "bedrock-titan-embed-v2" {
		t.Errorf("request model = %q, want %q", gotReq.Model, "bedrock-titan-embed-v2")
	}
	var inputStr string
	if err := json.Unmarshal(gotReq.Input, &inputStr); err != nil {
		t.Fatalf("single-text input should decode as string, got %s: %v", string(gotReq.Input), err)
	}
	if inputStr != "hello world" {
		t.Errorf("input = %q, want %q", inputStr, "hello world")
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
}

func TestOpenAICompatEmbedder_Embed_NoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization set when apiKey empty: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(openaiResponse([][]float32{{1, 2, 3}}))
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "")
	if _, err := e.Embed(context.Background(), "x"); err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
}

func TestOpenAICompatEmbedder_EmbedBatch(t *testing.T) {
	var gotReq openaiEmbedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		_ = json.NewEncoder(w).Encode(openaiResponse([][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}))
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "k")
	texts := []string{"a", "b", "c"}
	vecs, err := e.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch() error = %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("len(vecs) = %d, want 3", len(vecs))
	}
	if vecs[1][1] != 1 {
		t.Errorf("vec[1][1] = %v, want 1", vecs[1][1])
	}

	var inputList []string
	if err := json.Unmarshal(gotReq.Input, &inputList); err != nil {
		t.Fatalf("batch input should decode as []string, got %s: %v", string(gotReq.Input), err)
	}
	if len(inputList) != 3 || inputList[0] != "a" || inputList[2] != "c" {
		t.Errorf("input list = %v, want [a b c]", inputList)
	}
}

func TestOpenAICompatEmbedder_EmbedBatch_OutOfOrderIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Some providers return data out of order. The backend must rely on
		// the explicit `index` field, not the array position.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0, 1, 0}, "index": 1},
				{"embedding": []float32{1, 0, 0}, "index": 0},
				{"embedding": []float32{0, 0, 1}, "index": 2},
			},
		})
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "")
	vecs, err := e.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("EmbedBatch() error = %v", err)
	}
	if vecs[0][0] != 1 || vecs[1][1] != 1 || vecs[2][2] != 1 {
		t.Errorf("out-of-order data not reordered correctly: %v", vecs)
	}
}

func TestOpenAICompatEmbedder_EmbedBatch_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for empty batch")
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "")
	vecs, err := e.EmbedBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("EmbedBatch(nil) error = %v", err)
	}
	if vecs != nil {
		t.Errorf("EmbedBatch(nil) = %v, want nil", vecs)
	}
}

func TestOpenAICompatEmbedder_Embed_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "bad-key")
	_, err := e.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("Embed() expected 401 error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want status code in message", err)
	}
}

func TestOpenAICompatEmbedder_Embed_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "")
	_, err := e.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("Embed() expected 500 error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want status code in message", err)
	}
}

func TestOpenAICompatEmbedder_Embed_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "")
	_, err := e.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("Embed() expected error for empty response, got nil")
	}
}

func TestOpenAICompatEmbedder_EmbedBatch_LengthMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(openaiResponse([][]float32{{1, 0}}))
	}))
	defer srv.Close()

	e := NewOpenAICompatEmbedder(srv.URL, "m", "")
	_, err := e.EmbedBatch(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("EmbedBatch() expected length-mismatch error, got nil")
	}
}

func TestOpenAICompatEmbedder_Embed_ContextCanceled(t *testing.T) {
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(ready)
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})

	e := NewOpenAICompatEmbedder(srv.URL, "m", "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ready
		cancel()
	}()
	_, err := e.Embed(ctx, "x")
	if err == nil {
		t.Fatal("Embed() expected ctx error, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context") {
		t.Errorf("error = %v, want context error", err)
	}
}
