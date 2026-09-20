-- sqlite-migrate: checksum 019fae7180d3d0f4d80697eaaa5b56b31d8a134f9345c068a1f3c0f2a018fee7

ALTER TABLE schema_info ADD COLUMN update_channel text NOT NULL DEFAULT 'stable' CHECK (update_channel in ('stable', 'beta'));

ALTER TABLE schema_info ADD COLUMN update_check_enabled integer NOT NULL DEFAULT 1 CHECK (update_check_enabled in (0, 1));

ALTER TABLE schema_info ADD COLUMN previous_version text;
