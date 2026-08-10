package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// Complete makes exactly one attempt at req.Task, req.Tier: resolve the
// tier's configuration, call it, classify the outcome, log it, and return.
// See the package doc for what it deliberately does not do.
func (c *Client) Complete(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	tier, cfgErr := c.routing.tierFor(req.Task, req.Tier)
	if cfgErr != nil {
		c.record(ctx, req, "", nil, cfgErr, start)
		return Result{}, cfgErr
	}

	tctx, cancel := context.WithTimeout(ctx, tier.Timeout())
	defer cancel()

	wireReq := toWireRequest(tier.Model, c.routing.Tasks[req.Task].Thinking, req.Messages, req.Tools)
	body, err := json.Marshal(wireReq)
	if err != nil {
		mErr := &Error{Kind: KindMalformed, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: fmt.Errorf("encode request: %w", err)}
		c.record(ctx, req, tier.Model, nil, mErr, start)
		return Result{}, mErr
	}

	url := strings.TrimRight(tier.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(tctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cErr := &Error{Kind: KindConfig, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: fmt.Errorf("build request against base_url %q: %w", tier.BaseURL, err)}
		c.record(ctx, req, tier.Model, nil, cErr, start)
		return Result{}, cErr
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		kind := KindUnavailable
		if errors.Is(err, context.DeadlineExceeded) {
			kind = KindTimeout
		}
		hErr := &Error{Kind: kind, Task: req.Task, Tier: req.Tier, Model: tier.Model, Err: err}
		c.record(ctx, req, tier.Model, nil, hErr, start)
		return Result{}, hErr
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		rErr := &Error{Kind: KindUnavailable, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: fmt.Errorf("read response: %w", err)}
		c.record(ctx, req, tier.Model, nil, rErr, start)
		return Result{}, rErr
	}

	var wireResp chatResponse
	if err := json.Unmarshal(data, &wireResp); err != nil {
		mErr := &Error{Kind: KindMalformed, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)}
		c.record(ctx, req, tier.Model, nil, mErr, start)
		return Result{}, mErr
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		rlErr := &Error{Kind: KindRateLimited, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: apiErr(wireResp, resp.StatusCode)}
		c.record(ctx, req, tier.Model, nil, rlErr, start)
		return Result{}, rlErr

	case resp.StatusCode >= 500:
		uErr := &Error{Kind: KindUnavailable, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: apiErr(wireResp, resp.StatusCode)}
		c.record(ctx, req, tier.Model, nil, uErr, start)
		return Result{}, uErr

	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		mErr := &Error{Kind: KindMalformed, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: apiErr(wireResp, resp.StatusCode)}
		c.record(ctx, req, tier.Model, nil, mErr, start)
		return Result{}, mErr
	}

	if len(wireResp.Choices) == 0 || wireResp.Choices[0].Message.empty() || isRefusal(wireResp.Choices[0].FinishReason) {
		eErr := &Error{Kind: KindEmpty, Task: req.Task, Tier: req.Tier, Model: tier.Model,
			Err: emptyErr(wireResp)}
		c.record(ctx, req, tier.Model, nil, eErr, start)
		return Result{}, eErr
	}

	choice := wireResp.Choices[0]
	result := Result{
		Message:      fromWireMessage(choice.Message),
		Tier:         req.Tier,
		Model:        tier.Model,
		FinishReason: choice.FinishReason,
	}
	if wireResp.Usage != nil {
		result.PromptTokens = wireResp.Usage.PromptTokens
		result.CompletionTokens = wireResp.Usage.CompletionTokens
	}
	result.Latency = time.Since(start)

	c.record(ctx, req, tier.Model, &result, nil, start)
	return result, nil
}

// record writes exactly one llm_calls row and two metric observations for
// one Complete call, success or failure. It runs against a context detached
// from ctx (context.WithoutCancel) on the same reasoning scheduler.Fire's
// end-of-tick release does: the caller's context may already be cancelled or
// past its deadline by the time the model call finishes, and the point of
// this write is precisely to record that outcome, not to inherit its
// cancellation.
//
// A failure to write the row itself is logged and swallowed — it must never
// mask the real outcome of the model call, which is what Complete's return
// value already reports.
func (c *Client) record(ctx context.Context, req Request, model string, result *Result, callErr error, start time.Time) {
	latency := time.Since(start)
	ms := int(latency.Milliseconds())

	n := domain.NewLLMCall{
		Task:         string(req.Task),
		Tier:         req.Tier,
		Model:        model,
		LatencyMS:    &ms,
		OccurrenceID: req.OccurrenceID,
	}
	if req.Escalation != nil {
		n.Escalated = true
		reason := req.Escalation.Reason
		n.EscalationReason = &reason
	}

	outcome := "success"
	if result != nil {
		pt, ct := result.PromptTokens, result.CompletionTokens
		n.PromptTokens = &pt
		n.CompletionTokens = &ct
	}
	if callErr != nil {
		msg := callErr.Error()
		n.Error = &msg
		outcome = outcomeFor(callErr)
	}

	logCtx := context.WithoutCancel(ctx)
	if _, err := c.store.CreateLLMCall(logCtx, n); err != nil {
		c.log.Error("model: write llm_calls row failed", "task", req.Task, "tier", req.Tier, "error", err)
	}

	if c.metrics != nil {
		c.metrics.IncLLMCall(string(req.Task), req.Tier, outcome)
		c.metrics.ObserveLLMLatency(string(req.Task), req.Tier, latency.Seconds())
	}
}

func outcomeFor(err error) string {
	var mErr *Error
	if errors.As(err, &mErr) {
		return mErr.Kind.String()
	}
	return "error"
}

func apiErr(resp chatResponse, status int) error {
	if resp.Error != nil && resp.Error.Message != "" {
		return fmt.Errorf("http %d: %s", status, resp.Error.Message)
	}
	return fmt.Errorf("http %d", status)
}

func emptyErr(resp chatResponse) error {
	if len(resp.Choices) == 0 {
		return fmt.Errorf("no choices in response")
	}
	if reason := resp.Choices[0].FinishReason; reason != "" {
		return fmt.Errorf("finish_reason %q with no content or tool calls", reason)
	}
	return fmt.Errorf("empty completion")
}

func isRefusal(finishReason string) bool {
	return finishReason == "content_filter"
}
