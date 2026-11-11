BEGIN;

CREATE TABLE channel_event_heads (
    channel_id bigint PRIMARY KEY REFERENCES channels(id),
    last_seq bigint NOT NULL CHECK (last_seq > 0)
);

ALTER TABLE message_outbox
    ADD COLUMN channel_id bigint REFERENCES channels(id),
    ADD COLUMN channel_seq bigint;

WITH numbered AS (
    SELECT o.id, m.channel_id,
           row_number() OVER (PARTITION BY m.channel_id ORDER BY o.id) AS seq
    FROM message_outbox AS o
    JOIN messages AS m ON m.id = o.message_id
)
UPDATE message_outbox AS o
SET channel_id = numbered.channel_id, channel_seq = numbered.seq
FROM numbered
WHERE o.id = numbered.id;

INSERT INTO channel_event_heads (channel_id, last_seq)
SELECT channel_id, max(channel_seq)
FROM message_outbox
GROUP BY channel_id;

ALTER TABLE message_outbox
    ALTER COLUMN channel_id SET NOT NULL,
    ALTER COLUMN channel_seq SET NOT NULL,
    ADD CHECK (channel_seq > 0),
    ADD UNIQUE (channel_id, channel_seq);

COMMIT;
