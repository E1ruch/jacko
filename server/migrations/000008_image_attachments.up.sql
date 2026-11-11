BEGIN;

CREATE TABLE attachments (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    channel_id bigint NOT NULL REFERENCES channels(id),
    uploader_id bigint NOT NULL REFERENCES users(id),
    media_type text NOT NULL CHECK (media_type IN ('image/jpeg', 'image/png')),
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 8388608),
    upload_expires_at timestamptz NOT NULL,
    object_key text UNIQUE,
    width integer CHECK (width BETWEEN 1 AND 4096),
    height integer CHECK (height BETWEEN 1 AND 4096),
    CHECK ((object_key IS NULL AND width IS NULL AND height IS NULL)
        OR (object_key IS NOT NULL AND width IS NOT NULL AND height IS NOT NULL)),
    UNIQUE (channel_id, id)
);

ALTER TABLE messages
    DROP CONSTRAINT messages_content_check,
    ADD COLUMN attachment_id text UNIQUE,
    ADD CHECK (octet_length(content) <= 8192 AND (octet_length(content) > 0 OR attachment_id IS NOT NULL)),
    ADD FOREIGN KEY (channel_id, attachment_id) REFERENCES attachments(channel_id, id);

COMMIT;
