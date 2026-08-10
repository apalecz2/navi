-- name: CreateConversation :one
INSERT INTO conversations (
  id, role, content, tool_calls, tool_call_id, transport, external_id, context_ref, created_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetConversation :one
SELECT * FROM conversations
WHERE id = ?;

-- name: GetConversationByTransportExternalID :one
SELECT * FROM conversations
WHERE transport = ? AND external_id = ?;

-- name: ListRecentConversations :many
SELECT * FROM conversations
ORDER BY created_at DESC, id DESC
LIMIT ?;
