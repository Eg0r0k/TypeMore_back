-- +goose Up
-- +goose StatementBegin
CREATE TABLE lobbies (
    id UUID PRIMARY KEY,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL,
    is_public BOOLEAN NOT NULL,
    password BYTEA,
    status SMALLINT NOT NULL,
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE
    max_players INTEGER NOT NULL,
    name VARCHAR(255) NOT NULL,
    is_open BOOLEAN NOT NULL,
    players UUID[] NOT NULL
);

CREATE INDEX idx_lobbies_status ON lobbies(status);
CREATE INDEX idx_lobbies_is_public ON lobbies(is_public);
CREATE INDEX idx_lobbies_owner_id ON lobbies(owner_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS lobbies;
-- +goose StatementEnd