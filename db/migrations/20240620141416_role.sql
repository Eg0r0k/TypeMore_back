-- +goose Up
-- +goose StatementBegin
CREATE TABLE roles (
    id SMALLSERIAL PRIMARY KEY,
    name VARCHAR(255) UNIQUE NOT NULL
);

INSERT INTO roles (id, name) VALUES 
(-1, 'invalid'),
(0, 'user'),
(1, 'admin'),
(2, 'super_admin');

CREATE TABLE users (
   id UUID PRIMARY KEY,
   username VARCHAR(255) UNIQUE NOT NULL,
   email VARCHAR(255) UNIQUE NOT NULL,
   is_banned BOOLEAN DEFAULT FALSE,
   config TEXT,
   password BYTEA NOT NULL,
   created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
   updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
   last_in TIMESTAMP WITH TIME ZONE,
   last_out TIMESTAMP WITH TIME ZONE,
   registration_date TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE user_roles ( 
   user_id UUID REFERENCES users(id) ON DELETE CASCADE,
   role_id SMALLINT REFERENCES roles(id) ON DELETE CASCADE,
   PRIMARY KEY (user_id, role_id) 
);
CREATE TABLE password_reset_tokens (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY, 
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    token VARCHAR(255) NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    used BOOLEAN DEFAULT FALSE
);
CREATE INDEX idx_users_username ON users(username);
CREATE INDEX idx_users_email ON users(email);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS user_roles CASCADE; 
DROP TABLE IF EXISTS password_reset_tokens CASCADE;
DROP TABLE IF EXISTS users CASCADE; 
DROP TABLE IF EXISTS roles CASCADE;
-- +goose StatementEnd
