package hosting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ExternalAdapter struct {
	httpClient *http.Client
}

func NewExternalAdapter() *ExternalAdapter {
	return &ExternalAdapter{
		httpClient: &http.Client{
			Timeout: 0, // streaming handled via context
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return errors.New("redirects are prohibited")
			},
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
				}).DialContext,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
}

// ValidateDestination verifies endpoint destinations per architectural security rules.
// Denies metadata addresses, 0.0.0.0, and unencrypted off-machine plain HTTP.
func ValidateDestination(endpointURL string) error {
	u, err := url.Parse(endpointURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q; must be http or https", u.Scheme)
	}

	host := u.Hostname()
	if host == "" || host == "0.0.0.0" {
		return errors.New("invalid or empty host")
	}

	// Reject cloud metadata addresses (SSRF prevention)
	if host == "169.254.169.254" || strings.Contains(host, "metadata.google.internal") {
		return errors.New("cloud metadata services are strictly prohibited")
	}

	// Plain HTTP is limited to loopback or local container names
	if u.Scheme == "http" {
		isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
		isLocalName := !strings.Contains(host, ".") || host == "host.docker.internal" || host == "host.containers.internal"
		if !isLoopback && !isLocalName {
			return fmt.Errorf("plain HTTP is restricted to loopback/local targets; off-machine endpoints require HTTPS")
		}
	}

	return nil
}

func (a *ExternalAdapter) QueryModels(ctx context.Context, endpointURL string, apiKey string) ([]string, error) {
	if err := ValidateDestination(endpointURL); err != nil {
		return nil, fmt.Errorf("destination validation failed: %w", err)
	}

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

		resp, err := a.httpClient.Do(req)
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

func (a *ExternalAdapter) TestEndpoint(ctx context.Context, endpointURL string, apiKey string) (int64, []string, error) {
	start := time.Now()
	models, err := a.QueryModels(ctx, endpointURL, apiKey)
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
	endpointURL string,
	apiKey string,
	model string,
	messages []ChatMessage,
	onChunk func(delta string) error,
) error {
	if err := ValidateDestination(endpointURL); err != nil {
		return fmt.Errorf("destination validation failed: %w", err)
	}

	urlStr := strings.TrimRight(endpointURL, "/")
	if !strings.HasSuffix(urlStr, "/chat/completions") {
		urlStr = urlStr + "/v1/chat/completions"
	}

	reqBody := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   true,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := a.httpClient.Do(req)
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
