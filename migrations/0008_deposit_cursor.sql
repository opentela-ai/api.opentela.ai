CREATE TABLE IF NOT EXISTS deposit_cursors (
    treasury_ata    TEXT PRIMARY KEY,
    last_signature  TEXT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
