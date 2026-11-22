package main

import "time"

// tokens are the token counts an agent reported.
type tokens struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

func (t *tokens) add(o tokens) {
	t.InputTokens += o.InputTokens
	t.OutputTokens += o.OutputTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheWriteTokens += o.CacheWriteTokens
}

// modelUsage is one model's share of a step; CostUSD is nil when the agent reports no price (codex).
type modelUsage struct {
	Model string `json:"model"`
	tokens
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// agentUsage is what an agent reported about one run: the per-model entries and their sums.
// CostUSD nil means no price; a nil *agentUsage means no report, so the cost is unknown.
type agentUsage struct {
	Models []modelUsage `json:"models"`
	APIMs  int64        `json:"api_ms"`
	Turns  int          `json:"turns"`
	tokens
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// Step statuses in stepMetrics.
const statusCancelled = "cancelled" // besides statusDone and statusFailed

// Error kinds of a failed or cancelled step: fixed categories derived in Go, never agent text.
const (
	errTimeout    = "timeout"
	errCancelled  = "cancelled"
	errNotFound   = "not_found"
	errExit       = "exit"
	errAgentError = "agent_error"
	errBadOutput  = "bad_output"
	errOther      = "other"
)

type stepMetrics struct {
	Step       string      `json:"step"`
	Agent      string      `json:"agent"`
	PromptSHA  string      `json:"prompt_sha"` // hex SHA-256 of the resolved step prompt
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at"`
	WallMs     int64       `json:"wall_ms"`
	Status     string      `json:"status"`     // statusDone, statusFailed or statusCancelled
	ErrorKind  string      `json:"error_kind"` // set only when failed or cancelled
	Error      string      `json:"error"`      // the failure reason, at most maxReason characters
	Items      int         `json:"items"`      // findings, or areas plus points for risk assessment
	Usage      *agentUsage `json:"usage,omitempty"`
}

// reviewMetrics describes one workflow run. Totals are computed by readers, not stored.
type reviewMetrics struct {
	RunID      string        `json:"run_id"`
	QueuedAt   time.Time     `json:"queued_at"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Attempt    int           `json:"attempt"`
	DiffFiles  int           `json:"diff_files"`
	DiffBytes  int           `json:"diff_bytes"`
	Steps      []stepMetrics `json:"steps"`
}
