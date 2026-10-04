-- sqlite-migrate: checksum 4179ec41366be839cf22f5153da3465d7d8c4a587b3b92df69209ea18311896b

ALTER TABLE migration_session ADD COLUMN scan_full_checksums integer NOT NULL DEFAULT 0 CHECK (scan_full_checksums in (0, 1));
