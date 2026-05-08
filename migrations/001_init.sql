-- ymca-wellness-cafe-v2 initial schema
--
-- Run once against the database named in .env (DB_NAME).
--
--     psql -U postgres -d ymca_wellness_cafe_v2 -f migrations/001_init.sql

BEGIN;

-- ---------------------------------------------------------------------------
-- Status ledger for every dApp-initiated transaction (reward, add_activity,
-- add_admin, deploy, ...). One row per inbound request.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS transfer_status (
    request_id        VARCHAR(64)  PRIMARY KEY,        -- our own UUID
    transaction_id    VARCHAR(128),                    -- from /rubix/v1/signature
    kind              VARCHAR(32)  NOT NULL,           -- reward | add_activity | add_admin | deploy | create_did
    admin_did         VARCHAR(80),
    user_did          VARCHAR(80),
    activity_ids      TEXT[],                          -- null for non-reward kinds
    reward_points     INTEGER      DEFAULT 0,
    contract_hash     VARCHAR(128),                    -- SC token id used, if any
    status            VARCHAR(16)  NOT NULL DEFAULT 'queued',  -- queued|processing|success|failed
    message           TEXT,
    error_details     TEXT,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_transfer_status_admin_did  ON transfer_status(admin_did);
CREATE INDEX IF NOT EXISTS idx_transfer_status_user_did   ON transfer_status(user_did);
CREATE INDEX IF NOT EXISTS idx_transfer_status_status     ON transfer_status(status);
CREATE INDEX IF NOT EXISTS idx_transfer_status_tx_id      ON transfer_status(transaction_id);
CREATE INDEX IF NOT EXISTS idx_transfer_status_created_at ON transfer_status(created_at DESC);

-- ---------------------------------------------------------------------------
-- Username / external-id -> DID registry.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_did_registry (
    username    VARCHAR(128) PRIMARY KEY,
    did         VARCHAR(80)  NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_did_registry_did ON user_did_registry(did);

-- ---------------------------------------------------------------------------
-- Per-admin smart-contract catalog.
--
-- Rubix v2 smart-contract token ids embed the deploying node's PeerID, so
-- the same (wasm, rs) deployed from two different admin nodes produces
-- two different ids. We therefore track contracts per admin.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS admin_contracts (
    admin_did       VARCHAR(80)  NOT NULL,
    contract_kind   VARCHAR(32)  NOT NULL,  -- add_activity | add_admin | ...
    contract_hash   VARCHAR(128) NOT NULL,
    deployed_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (admin_did, contract_kind)
);

CREATE INDEX IF NOT EXISTS idx_admin_contracts_hash ON admin_contracts(contract_hash);

-- ---------------------------------------------------------------------------
-- dApp-side mirror of activity state.
--
-- The v2 Rubix node does not execute WASM; SC `data` is opaque passthrough.
-- We therefore persist activity records locally and treat the chain entry
-- as an audit log.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS activities (
    admin_did     VARCHAR(80)  NOT NULL,
    activity_id   VARCHAR(128) NOT NULL,
    reward_points INTEGER      NOT NULL DEFAULT 0,
    description   TEXT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (admin_did, activity_id)
);

COMMIT;
