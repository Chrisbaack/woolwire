package hosting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type RunnerHealth struct {
	Status        string `json:"status"`
	LoadedModelID string `json:"loaded_model_id"`
	LoadedFile    string `json:"loaded_file"`
	EnginePID     int    `json:"engine_pid"`
}

type RunnerClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewRunnerClient(baseURL, token string) *RunnerClient {
	return &RunnerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: 0,
		},
	}
}

func (c *RunnerClient) doJSON(ctx context.Context, method, path string, in any, out any) error {
	var bodyReader *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("runner request %s failed: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("runner error (%d)", resp.StatusCode)
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *RunnerClient) Health(ctx context.Context) (*RunnerHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var h RunnerHealth
	if err := c.doJSON(ctx, "GET", "/runner/v1/health", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (c *RunnerClient) LoadModel(ctx context.Context, modelID, filename string, ctxLimit, threads, gpuLayers int) error {
	req := map[string]any{
		"model_id":      modelID,
		"filename":      filename,
		"context_limit": ctxLimit,
		"threads":       threads,
		"gpu_layers":    gpuLayers,
	}
	return c.doJSON(ctx, "POST", "/runner/v1/models/load", req, nil)
}

func (c *RunnerClient) UnloadModel(ctx context.Context) error {
	return c.doJSON(ctx, "POST", "/runner/v1/models/unload", nil, nil)
}

func (c *RunnerClient) RestartEngine(ctx context.Context) error {
	return c.doJSON(ctx, "POST", "/runner/v1/engine/restart", nil, nil)
}

// StreamChat drives a managed model through the runner companion. The
// requester is named in the body so the runner can restart the engine between
// different members, which is what keeps one member's KV cache out of the
// next member's session.
func (c *RunnerClient) StreamChat(
	ctx context.Context,
	requesterMemberID string,
	modelName string,
	maxTokens int,
	messages []ChatMessage,
	onChunk func(delta string) error,
) error {
	payload := map[string]any{
		"model":               modelName,
		"messages":            messages,
		"stream":              true,
		"requester_member_id": requesterMemberID,
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/runner/v1/inference", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("inference request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runner returned status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var chunk chatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
					if err := onChunk(chunk.Choices[0].Delta.Content); err != nil {
						return err
					}
				}
			}
		}
	}
	return scanner.Err()
}

func (c *RunnerClient) CancelInference(ctx context.Context) error {
	return c.doJSON(ctx, "POST", "/runner/v1/inference/cancel", nil, nil)
}
