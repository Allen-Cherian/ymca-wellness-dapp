-- 002_admins.sql
-- Admins are now sourced from the database (replacing config.toml).
-- Provision via POST /api/admins/setup.

CREATE TABLE IF NOT EXISTS admins (
    did         VARCHAR(80)  PRIMARY KEY,
    node_port   VARCHAR(16)  NOT NULL,
    password    VARCHAR(64)  NOT NULL DEFAULT 'mypassword',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_admins_node_port ON admins(node_port);
