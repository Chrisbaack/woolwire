package hosting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxRunnerErrorBytes bounds how much of a runner error body is quoted back.
const maxRunnerErrorBytes = 4 << 10

type RunnerHealth struct {
	Status        string `json:"status"`
	LoadedModelID string `json:"loaded_model_id"`
	LoadedFile    string `json:"loaded_file"`
	EnginePID     int    `json:"engine_pid"`
	// HasGPU and GPUName describe the runner's hardware. The app cannot detect
	// it: the GPU is passed to the runner container, not to this one.
	HasGPU  bool   `json:"has_gpu"`
	GPUName string `json:"gpu_name"`
	// Error is why the engine is in an error state, including what it printed
	// before exiting.
	Error string `json:"error"`
	// Notice explains a load that succeeded on different terms than asked.
	Notice               string     `json:"notice"`
	ContextLimit         int        `json:"context_limit"`
	Threads              int        `json:"threads"`
	GPULayers            *int       `json:"gpu_layers"`
	ExtraArgs            []string   `json:"extra_args"`
	Processing           bool       `json:"processing"`
	QueuedRequests       int        `json:"queued_requests"`
	RequestsCompleted    int        `json:"requests_completed"`
	RequestsFailed       int        `json:"requests_failed"`
	RequestsCancelled    int        `json:"requests_cancelled"`
	PromptTokens         int        `json:"prompt_tokens"`
	CompletionTokens     int        `json:"completion_tokens"`
	LastPromptTokens     *int       `json:"last_prompt_tokens"`
	LastCompletionTokens *int       `json:"last_completion_tokens"`
	LastTokensPerSecond  *float64   `json:"last_tokens_per_second"`
	CurrentContextTokens *int       `json:"current_context_tokens"`
	StartedAt            *time.Time `json:"started_at"`
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
		// The runner explains itself in the body — which model file it could
		// not find, what the engine printed before giving up. Reporting only
		// the status code turned every one of those into "runner error (500)".
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxRunnerErrorBytes))
		if msg := strings.TrimSpace(string(detail)); msg != "" {
			return fmt.Errorf("runner error (%d): %s", resp.StatusCode, msg)
		}
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

// LoadRequest is one model to launch, with the companions that belong to it.
type LoadRequest struct {
	ModelID      string
	Filename     string
	ContextLimit int
	Threads      int
	// GPULayers nil leaves the offload decision to the runner, which is the
	// container the GPU is attached to.
	GPULayers *int
	// Projector and DraftModel are the model's companions, relative to the
	// models directory.
	Projector  string
	DraftModel string
	ExtraArgs  []string
}

// LoadModel asks the runner to launch an engine.
func (c *RunnerClient) LoadModel(ctx context.Context, load LoadRequest) error {
	req := map[string]any{
		"model_id":      load.ModelID,
		"filename":      load.Filename,
		"context_limit": load.ContextLimit,
		"threads":       load.Threads,
	}
	if load.GPULayers != nil {
		req["gpu_layers"] = *load.GPULayers
	}
	if load.Projector != "" {
		req["mmproj"] = load.Projector
	}
	if load.DraftModel != "" {
		req["draft_model"] = load.DraftModel
	}
	if load.ExtraArgs != nil {
		req["extra_args"] = load.ExtraArgs
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
	modelID string,
	modelName string,
	maxTokens int,
	messages []ChatMessage,
	thinking string,
	onChunk func(delta string) error,
) error {
	payload := map[string]any{
		"model":               modelName,
		"model_id":            modelID,
		"messages":            messages,
		"stream":              true,
		"requester_member_id": requesterMemberID,
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	if thinking != "" {
		payload["thinking"] = thinking
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

	wrapper := newReasoningWrapper(onChunk)
	scanner := bufio.NewScanner(resp.Body)
	// A reasoning model can emit a long single-line frame; the default 64 KiB
	// token limit would end the stream early with ErrTooLong.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var chunk chatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				if len(chunk.Choices) > 0 {
					delta := chunk.Choices[0].Delta
					reasoning := delta.ReasoningContent
					if reasoning == "" {
						reasoning = delta.Reasoning
					}
					if err := wrapper.emit(delta.Content, reasoning); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return wrapper.closeThink()
}

func (c *RunnerClient) CancelInference(ctx context.Context) error {
	return c.doJSON(ctx, "POST", "/runner/v1/inference/cancel", nil, nil)
}
