-- Persist customer language (one-time selection, then only change via /language).
CREATE TABLE IF NOT EXISTS customer_users (
    tg_user_id BIGINT NOT NULL PRIMARY KEY,
    language TEXT CHECK (language IN ('uz', 'ru')),
    language_selected_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_customer_users_language ON customer_users(language);
