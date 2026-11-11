BEGIN;

-- The unique (channel_id, id) index also contains tombstones. A channel with
-- many deleted messages must not scan them to return a page of live history.
CREATE INDEX messages_live_history_idx ON messages (channel_id, id DESC)
    WHERE deleted_at IS NULL;

COMMIT;
