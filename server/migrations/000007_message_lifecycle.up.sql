BEGIN;

ALTER TABLE messages
    ADD COLUMN reply_to bigint,
    ADD COLUMN edited_at timestamptz,
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN original_content_hash bytea,
    ALTER COLUMN content DROP NOT NULL;

UPDATE messages SET original_content_hash = sha256(convert_to(content, 'UTF8'));

ALTER TABLE messages
    ALTER COLUMN original_content_hash SET NOT NULL,
    ADD CHECK (octet_length(original_content_hash) = 32),
    ADD CHECK ((content IS NULL) = (deleted_at IS NOT NULL)),
    ADD UNIQUE (channel_id, id),
    ADD FOREIGN KEY (channel_id, reply_to) REFERENCES messages(channel_id, id);

DROP INDEX messages_channel_history_idx;

ALTER TABLE message_outbox DROP CONSTRAINT message_outbox_message_id_event_type_key;
CREATE INDEX message_outbox_message_idx ON message_outbox (message_id);

COMMIT;
