-- 003_user_admins.sql
-- Replaces user_did_registry (username -> DID) with user_admins
-- (user_did -> admin_did). One admin per user.
--
-- Populated as a side effect of POST /api/create-did-with-pubkey, and
-- read by GET /api/users/:user_did/admin.

CREATE TABLE IF NOT EXISTS user_admins (
    user_did    VARCHAR(80)  PRIMARY KEY,
    admin_did   VARCHAR(80)  NOT NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_admins_admin_did ON user_admins(admin_did);

-- Backfill from existing successful reward transfers. For users who got
-- rewards from multiple admins, take the earliest admin (deterministic).
INSERT INTO user_admins (user_did, admin_did, created_at)
SELECT DISTINCT ON (user_did)
       user_did,
       admin_did,
       created_at
FROM transfer_status
WHERE user_did != ''
  AND admin_did != ''
  AND kind = 'reward'
  AND status = 'success'
ORDER BY user_did, created_at ASC
ON CONFLICT (user_did) DO NOTHING;

-- Drop the obsolete username->DID table.
DROP TABLE IF EXISTS user_did_registry;
