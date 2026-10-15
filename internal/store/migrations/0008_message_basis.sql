-- 0008_message_basis: what a generated message was written from.
--
-- The copywriter's second pass (G3) runs "only if relevant state changed since
-- pass 1", and nothing on the row recorded what pass 1 saw. message_basis is
-- that record: a short fingerprint of the context the text was generated from
-- (the tone strategy, the streak numbers, the recent outcomes, the skip notes,
-- the item's title and notes, whether the row is a snooze child). Pass 2 rebuilds
-- the context, fingerprints it the same way, and calls the model only when the
-- two differ.
--
-- Chosen over comparing against message_generated_at because "what changed" is
-- not a timestamp: a pause moving has none, a skip note arrives on a different
-- row, and a state that changed and changed back is not a change. Equality of
-- inputs answers the question directly, and survives any surface writing the
-- change.
--
-- NULL on every row that has no generated text, and on a row generated before
-- this migration; pass 2 treats NULL as "changed".

ALTER TABLE occurrences ADD COLUMN message_basis TEXT;
