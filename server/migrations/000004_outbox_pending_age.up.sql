CREATE INDEX message_outbox_pending_created_at_idx
    ON message_outbox (created_at)
    WHERE published_at IS NULL;
