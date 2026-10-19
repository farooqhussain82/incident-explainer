package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const systemPrompt = `You are a senior SRE helping an on-call engineer.
You get diagnostic output from a Kubernetes pod, a log file, or an infrastructure change plan.
Explain what is wrong in plain English and say what to check next.

Rules:
- Only use facts that appear in the input. If the input is not enough, say so and lower confidence.
- Quote the exact lines that support your answer in "evidence".
- "next_steps" are concrete commands or checks, most useful first, at most 5.
- Never suggest deleting data, force-deleting resources, or other destructive commands
  without clearly saying they are destructive.
- Values like [REDACTED-...] were removed on purpose. Do not guess them.
- Keep "summary" to one or two sentences a manager could understand.`

var answerSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"summary":      map[string]any{"type": "string"},
		"likely_cause": map[string]any{"type": "string"},
		"evidence":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"next_steps":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"confidence":   map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
	},
	"required": []string{"summary", "likely_cause", "evidence", "next_steps", "confidence"},
}

// Explanation is what the model returns, plus some stats about the call.
type Explanation struct {
	Summary      string   `json:"summary"`
	LikelyCause  string   `json:"likely_cause"`
	Evidence     []string `json:"evidence"`
	NextSteps    []string `json:"next_steps"`
	Confidence   string   `json:"confidence"`
	Model        string   `json:"model"`
	Seconds      float64  `json:"seconds"`
	PromptTokens int      `json:"prompt_tokens"`
	OutputTokens int      `json:"output_tokens"`
}

type LLM struct {
	URL   string
	Model string
	http  *http.Client
}

func NewLLM(url, model string) *LLM {
	return &LLM{URL: strings.TrimRight(url, "/"), Model: model, http: &http.Client{Timeout: 10 * time.Minute}}
}

// Explain sends already-redacted text to the local Ollama server.
func (l *LLM) Explain(ctx context.Context, source, text string) (*Explanation, error) {
	body, _ := json.Marshal(map[string]any{
		"model":   l.Model,
		"stream":  false,
		"think":   false,
		"format":  answerSchema,
		"options": map[string]any{"temperature": 0.2, "num_ctx": 8192},
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": "Source: " + source + "\n\n" + text},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Ollama at %s: %w", l.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		TotalDuration   int64 `json:"total_duration"`
		PromptEvalCount int   `json:"prompt_eval_count"`
		EvalCount       int   `json:"eval_count"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	var ex Explanation
	if err := json.Unmarshal([]byte(out.Message.Content), &ex); err != nil {
		return nil, fmt.Errorf("model did not return valid JSON: %w", err)
	}
	ex.Model = l.Model
	ex.Seconds = float64(out.TotalDuration/1e8) / 10
	ex.PromptTokens = out.PromptEvalCount
	ex.OutputTokens = out.EvalCount
	return &ex, nil
}
