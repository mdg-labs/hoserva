-- sqlite-migrate: checksum e89412e0a5e5d7b0d52d4dc430ffe4ff4dbd4243f4d724b72c5c306b37e48e42

ALTER TABLE array_settings ADD COLUMN initial_sync_owed integer NOT NULL DEFAULT 0 CHECK (initial_sync_owed in (0, 1));
