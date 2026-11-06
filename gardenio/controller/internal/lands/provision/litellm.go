package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LiteLLM is the Keys implementation against a LiteLLM proxy's key admin API,
// authenticated with the master key the controller already holds for key
// rotation and spend sync.
type LiteLLM struct {
	BaseURL   string
	MasterKey string
	// HTTP defaults to a client with a 15s timeout.
	HTTP *http.Client
}

// errKeyNotFound is LiteLLM saying it does not know a key.
var errKeyNotFound = errors.New("litellm: key not found")

// Ensure creates the key unless LiteLLM already knows it. The key value is
// the caller's, so a create that is retried after a crash makes the same key,
// never a second one.
func (l *LiteLLM) Ensure(ctx context.Context, key, alias string, maxBudgetUSD float64, metadata map[string]string) error {
	err := l.call(ctx, http.MethodGet, "/key/info?key="+url.QueryEscape(key), nil, key)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, errKeyNotFound):
		return err
	}
	return l.call(ctx, http.MethodPost, "/key/generate", map[string]any{
		"key": key, "key_alias": alias, "max_budget": maxBudgetUSD, "metadata": metadata,
	}, key)
}

// SetBudget sets the key's max_budget in US dollars.
func (l *LiteLLM) SetBudget(ctx context.Context, key string, maxBudgetUSD float64) error {
	return l.call(ctx, http.MethodPost, "/key/update", map[string]any{"key": key, "max_budget": maxBudgetUSD}, key)
}

// Delete revokes the key; one LiteLLM does not know is already revoked.
func (l *LiteLLM) Delete(ctx context.Context, key string) error {
	err := l.call(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": []string{key}}, key)
	if errors.Is(err, errKeyNotFound) {
		return nil
	}
	return err
}

// Block makes the key refuse every call until Unblock; its max_budget and
// spend are kept. One LiteLLM does not know has nothing to block.
func (l *LiteLLM) Block(ctx context.Context, key string) error {
	return l.ignoreNotFound(l.call(ctx, http.MethodPost, "/key/block", map[string]any{"key": key}, key))
}

// Unblock lifts Block. One LiteLLM does not know has nothing to unblock.
func (l *LiteLLM) Unblock(ctx context.Context, key string) error {
	return l.ignoreNotFound(l.call(ctx, http.MethodPost, "/key/unblock", map[string]any{"key": key}, key))
}

func (l *LiteLLM) ignoreNotFound(err error) error {
	if errors.Is(err, errKeyNotFound) {
		return nil
	}
	return err
}

// call sends one request. Nothing it returns carries the key: LiteLLM echoes
// keys in bodies and the key rides in the info query string.
func (l *LiteLLM) call(ctx context.Context, method, path string, body any, key string) error {
	if strings.TrimSpace(l.BaseURL) == "" || strings.TrimSpace(l.MasterKey) == "" {
		return errors.New("litellm: LITELLM_URL and LITELLM_MASTER_KEY are required")
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	op := method + " " + strings.SplitN(path, "?", 2)[0]
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(l.BaseURL, "/")+path, rdr)
	if err != nil {
		return fmt.Errorf("litellm: %s: building the request", op)
	}
	req.Header.Set("Authorization", "Bearer "+l.MasterKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := l.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("litellm: %s: %s", op, redact(err.Error(), key))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg := redact(strings.TrimSpace(string(raw)), key)
	low := strings.ToLower(msg)
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode == http.StatusBadRequest &&
		(strings.Contains(low, "not found") || strings.Contains(low, "does not exist"))) {
		return fmt.Errorf("%w: %s", errKeyNotFound, op)
	}
	return fmt.Errorf("litellm: %s: HTTP %d: %s", op, resp.StatusCode, truncate(msg, 200))
}

// redact removes key from s.
func redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}
