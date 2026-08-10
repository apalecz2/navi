-- name: CreateLLMCall :one
INSERT INTO llm_calls (
  id, task, tier, model, prompt_tokens, completion_tokens, latency_ms,
  escalated, escalation_reason, error, occurrence_id, created_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: ListLLMCalls :many
SELECT * FROM llm_calls
ORDER BY created_at DESC, id DESC
LIMIT ?;

-- name: DeleteLLMCallsOlderThan :execrows
DELETE FROM llm_calls
WHERE created_at < ?;
