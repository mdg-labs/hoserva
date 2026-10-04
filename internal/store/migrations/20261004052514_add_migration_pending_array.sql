-- sqlite-migrate: checksum da9ca02793f394a447fe4961065bcbe9abe527e8ac591b2627eab88ddb132409

ALTER TABLE array_disks ADD COLUMN mount_source text;

ALTER TABLE array_settings ADD COLUMN migration_pending integer NOT NULL DEFAULT 0 CHECK (migration_pending in (0, 1));

ALTER TABLE array_settings ADD COLUMN migration_recorded text NOT NULL DEFAULT '';
