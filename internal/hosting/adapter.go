package hosting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest carries everything the adapter needs for one streamed
// completion. It is a struct rather than a parameter list because the model
// name, token ceiling, and network policy all travel together and forgetting
// one of them is exactly the class of bug this replaces.
type ChatRequest struct {
	EndpointURL         string
	APIKey              string
	Model               string
	MaxTokens           int
	Messages            []ChatMessage
	AllowPrivateNetwork bool
}

// ExternalAdapter talks to owner-configured OpenAI-compatible endpoints. It
// keeps one HTTP client per destination policy so the SSRF guard that applies
// to a given endpoint is the one baked into the transport that dials it.
type ExternalAdapter struct {
	mu      sync.Mutex
	clients map[DestinationPolicy]*http.Client
}

func NewExternalAdapter() *ExternalAdapter {
	return &ExternalAdapter{clients: make(map[DestinationPolicy]*http.Client)}
}

func (a *ExternalAdapter) clientFor(policy DestinationPolicy) *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clients == nil {
		a.clients = make(map[DestinationPolicy]*http.Client)
	}
	if c, ok := a.clients[policy]; ok {
		return c
	}
	c := guardedClient(policy, 30*time.Second)
	a.clients[policy] = c
	return c
}

func (a *ExternalAdapter) QueryModels(ctx context.Context, endpointURL string, apiKey string, policy DestinationPolicy) ([]string, error) {
	if err := ValidateDestinationWithPolicy(endpointURL, policy); err != nil {
		return nil, fmt.Errorf("destination validation failed: %w", err)
	}
	if err := requirePlainHTTPIsLocal(endpointURL, policy); err != nil {
		return nil, err
	}
	client := a.clientFor(policy)

	baseURL := strings.TrimRight(endpointURL, "/")
	// Candidates to check: /models (OpenAI standard) and /api/tags (Ollama native)
	candidates := []string{}
	if strings.HasSuffix(baseURL, "/v1") {
		candidates = append(candidates, baseURL+"/models", strings.TrimSuffix(baseURL, "/v1")+"/api/tags")
	} else {
		candidates = append(candidates, baseURL+"/v1/models", baseURL+"/models", baseURL+"/api/tags")
	}

	var lastErr error
	for _, targetURL := range candidates {
		req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			continue
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("endpoint %s returned HTTP %d", targetURL, resp.StatusCode)
			continue
		}

		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		// Try OpenAI format: {"data": [{"id": "model-id"}]}
		var openAIResp struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(bodyBytes, &openAIResp); err == nil && len(openAIResp.Data) > 0 {
			var models []string
			for _, m := range openAIResp.Data {
				name := m.ID
				if name == "" {
					name = m.Name
				}
				if name != "" {
					models = append(models, name)
				}
			}
			if len(models) > 0 {
				sort.Strings(models)
				return models, nil
			}
		}

		// Try Ollama format: {"models": [{"name": "llama3:latest"}]}
		var ollamaResp struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.Unmarshal(bodyBytes, &ollamaResp); err == nil && len(ollamaResp.Models) > 0 {
			var models []string
			for _, m := range ollamaResp.Models {
				name := m.Name
				if name == "" {
					name = m.Model
				}
				if name != "" {
					models = append(models, name)
				}
			}
			if len(models) > 0 {
				sort.Strings(models)
				return models, nil
			}
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no models found at endpoint (tried /v1/models and /api/tags)")
}

func (a *ExternalAdapter) TestEndpoint(ctx context.Context, endpointURL string, apiKey string, policy DestinationPolicy) (int64, []string, error) {
	start := time.Now()
	models, err := a.QueryModels(ctx, endpointURL, apiKey, policy)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return latency, nil, err
	}
	return latency, models, nil
}

type chatCompletionChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func (a *ExternalAdapter) StreamChat(
	ctx context.Context,
	req ChatRequest,
	onChunk func(delta string) error,
) error {
	policy := DestinationPolicy{AllowPrivateNetwork: req.AllowPrivateNetwork}
	if err := ValidateDestinationWithPolicy(req.EndpointURL, policy); err != nil {
		return fmt.Errorf("destination validation failed: %w", err)
	}
	if err := requirePlainHTTPIsLocal(req.EndpointURL, policy); err != nil {
		return err
	}

	urlStr := strings.TrimRight(req.EndpointURL, "/")
	if !strings.HasSuffix(urlStr, "/chat/completions") {
		urlStr = urlStr + "/v1/chat/completions"
	}

	reqBody := map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   true,
	}
	if req.MaxTokens > 0 {
		reqBody["max_tokens"] = req.MaxTokens
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	resp, err := a.clientFor(policy).Do(httpReq)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("external endpoint returned HTTP %d: %s", resp.StatusCode, string(errBytes))
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var chunk chatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				if err := onChunk(chunk.Choices[0].Delta.Content); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
