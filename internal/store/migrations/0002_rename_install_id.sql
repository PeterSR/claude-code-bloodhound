-- 0002: rename meta key install_id -> device_id.
--
-- The conceptual model evolved: install_id was overloaded as both
-- "this install" and "this user's identity." We now distinguish device
-- (random per install, persisted here) from user (optional, in
-- config.json, pasted across machines to link them). The store's job
-- is just the device half.
--
-- Idempotent: a fresh install never had the old key, and a re-applied
-- migration finds nothing to rename.

UPDATE meta SET key = 'device_id' WHERE key = 'install_id';
