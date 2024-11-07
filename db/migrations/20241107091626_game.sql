-- +goose Up
-- +goose StatementBegin
CREATE TABLE game_modes(
    id SERIAL PRIMARY KEY,
    name VARCHAR(50),
    description TEXT
);
CREATE TABLE game_settings(
    id SERIAL PRIMARY KEY, 
    mode_id INT REFERENCES game_modes(id) ON DELETE CASCADE, 
    settings_type VARCHAR(50) NOT NULL, 
    value INT NOT NULL,
    is_custom BOOLEAN DEFAULT FALSE,
    create_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP, 

);
CREATE TABLE game_text(
    id UUID PRIMARY KEY,
    content TEXT NOT NULL,
    type VARCHAR(50) NOT NULL CHECK (type IN('random', 'qoute'))
);
CREATE TABLE games( 
    id UUID PRIMARY KEY, 
    user_id UUID REFERENCES users(id) ON DELETE CASCADE, 
    wpm DECIMAL(5,2) NOT NULL, 
    accuracy DECIMAL(5,2) CHECK (accuracy >= 0 AND accuracy <= 100),
    duration_seconds DECIMAL(5,2),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP, 
    is_finished BOOLEAN DEFAULT FALSE, 
    score INT DEFAULT 0, 
    setting_id INT REFERENCES game_settings(id) ON DELETE SET NULL,
    text_id UUID REFERENCES game_text(id) ON DELETE SET NULL
);
CREATE INDEX idx_games_users_id ON games(user_id);
CREATE INDEX idx_game_text_type ON game_text(type);
CREATE INDEX idx_game_settings_mode_id ON game_settings(mode_id);
CREATE INDEX idx_game_settings_type ON game_settings(settings_type);
CREATE INDEX idx_games_setting_id ON games(setting_id);
CREATE INDEX idx_games_text_id ON games(text_id);
CREATE INDEX idx_games_created_at ON games(created_at);
CREATE INDEX idx_games_is_finished ON games(is_finished);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS game_modes;
DROP TABLE IF EXISTS game_settings;
DROP TABLE IF EXISTS game_text;
DROP TABLE IF EXISTS games;
-- +goose StatementEnd
