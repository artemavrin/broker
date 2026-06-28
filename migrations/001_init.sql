-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE initiators (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    secret_hash  BYTEA   NOT NULL,            -- sha256(secret), 32 bytes
    revoked      BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX initiators_secret_hash_idx ON initiators (secret_hash);

CREATE TABLE receivers (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    initiator_id UUID    NOT NULL REFERENCES initiators(id) ON DELETE CASCADE,
    secret_hash  BYTEA   NOT NULL,
    revoked      BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX receivers_secret_hash_idx ON receivers (secret_hash);
CREATE INDEX receivers_initiator_idx ON receivers (initiator_id);

CREATE TABLE messages (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    from_addr     UUID    NOT NULL,           -- set by the broker from JWT
    to_addr       UUID    NOT NULL,           -- validated by policy
    payload       BYTEA   NOT NULL,           -- opaque; the broker never parses it
    client_msg_id TEXT,                       -- send idempotency
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until  TIMESTAMPTZ,                -- visibility timeout
    attempts      INT     NOT NULL DEFAULT 0
);
-- inbox selection
CREATE INDEX messages_inbox_idx ON messages (to_addr, id);
-- idempotent send (partial: only when client_msg_id is provided)
CREATE UNIQUE INDEX messages_idem_idx
    ON messages (from_addr, client_msg_id)
    WHERE client_msg_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS receivers;
DROP TABLE IF EXISTS initiators;
-- +goose StatementEnd
