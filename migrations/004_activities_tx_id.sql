-- 004_activities_tx_id.sql
-- Adds the on-chain transaction_id (the tx that wrote the activity to
-- the add_activity contract chain) to each activity row. Surfaced as
-- `block_hash` in the v1 /admin/activity/list response.
--
-- Existing rows are left NULL — they were created before this column
-- existed and there's no reliable way to recover their tx ids.

ALTER TABLE activities ADD COLUMN IF NOT EXISTS transaction_id VARCHAR(128);
