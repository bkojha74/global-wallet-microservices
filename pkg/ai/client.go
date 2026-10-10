package ai

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

// Client wraps the Gemini AI client for this project.
// When GEMINI_API_KEY is not set, Client is nil and all AI features
// are disabled automatically — the rest of the system is unaffected.
type Client struct {
	inner   *genai.Client
	model   string
	timeout time.Duration
}

// NewClient reads GEMINI_API_KEY from the environment and creates a client.
// Returns nil (not an error) when the key is absent — AI is disabled gracefully.
// This matches the pattern used by parseAuthConfig() in cmd/auth-service/main.go.
func NewClient(ctx context.Context) (*Client, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil, nil // AI disabled — intentional, not an error
	}

	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-1.5-flash"
	}

	timeoutMS := 5000
	if v := os.Getenv("AI_INFERENCE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutMS = n
		}
	}

	inner, err := genai.NewClient(ctx, option.WithAPIKey(key))
	if err != nil {
		return nil, fmt.Errorf("ai.NewClient: %w", err)
	}

	return &Client{
		inner:   inner,
		model:   model,
		timeout: time.Duration(timeoutMS) * time.Millisecond,
	}, nil
}

// IsEnabled returns true when the client has a valid Gemini connection.
// Always check this before calling any AI methods.
func (c *Client) IsEnabled() bool {
	return c != nil && c.inner != nil
}

// Close releases the underlying HTTP connection pool. Safe to call on nil.
func (c *Client) Close() error {
	if c == nil || c.inner == nil {
		return nil
	}
	return c.inner.Close()
}

// Generate sends a plain-text prompt to Gemini and returns the response text.
// It applies the client's own timeout so callers must not add another timeout.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	if !c.IsEnabled() {
		return "", fmt.Errorf("ai: client not initialised (GEMINI_API_KEY not set)")
	}

	tctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	m := c.inner.GenerativeModel(c.model)
	m.SetTemperature(0.1) // low temperature = consistent, deterministic JSON output

	resp, err := m.GenerateContent(tctx, genai.Text(prompt))
	if err != nil {
		return "", fmt.Errorf("GenerateContent: %w", err)
	}
	if len(resp.Candidates) == 0 {
		return "", fmt.Errorf("gemini returned an empty response")
	}

	part := resp.Candidates[0].Content.Parts[0]
	text, ok := part.(genai.Text)
	if !ok {
		return "", fmt.Errorf("unexpected Gemini response type: %T", part)
	}
	return string(text), nil
}
