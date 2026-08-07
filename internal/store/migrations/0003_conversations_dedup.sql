-- 0003_conversations_dedup: Telegram (and any future webhook adapter)
-- redelivers an update it did not get a 2xx for, so the same update can
-- arrive twice. store.CreateConversation already guards this by checking
-- before it inserts, inside the one writer transaction — this index is the
-- backstop, on the same "guard belongs in SQL, not caller discipline"
-- reasoning idx_occ_due and the DeleteFuturePendingOccurrence WHERE clause
-- already use.
--
-- Partial: a row this service generated itself (an assistant reply, a tool
-- result) has no transport/external_id and is never a dedup candidate.

CREATE UNIQUE INDEX idx_conv_dedup ON conversations(transport, external_id)
  WHERE transport IS NOT NULL AND external_id IS NOT NULL;
