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

-- LatestContextRef is the most recent context_ref matching a prefix pattern -
-- 'reconcile:%' for a check-in, and the same shape for the briefing P3.5 adds.
-- It is what names the open question in the agent's Context ref block: the
-- reply to a 21:00 check-in read at 00:15 is answering reconcile:{yesterday},
-- and only the row itself knows that.
--
-- This is the first reader context_ref has ever had. It was written from the
-- session the reconciler started sending, and left unread until there was a
-- reply path to read it for.
--
-- Whether a check-in is still open is deliberately not decided here.
-- ListAwaitingReconciliation answers that, from the occurrences themselves,
-- and the caller renders nothing when that set is empty. Asking a conversations
-- row whether its question still stands would be a second answer to a question
-- the occurrence rows already answer, and the two would eventually disagree.
--
-- No index on context_ref, and none is warranted: conversations is capped by
-- 180-day retention and this runs once per turn, against a table the ladder
-- already scans for its own history.
--
-- name: LatestContextRef :one
SELECT context_ref FROM conversations
WHERE context_ref LIKE ?
ORDER BY created_at DESC, id DESC
LIMIT 1;
