-- sqlite-migrate: checksum 8244ded524c7d5a7de534dda9dcd6debb59daf5b84679171825c5573792c46a3

ALTER TABLE array_settings ADD COLUMN migration_finished_at text NOT NULL DEFAULT '';

ALTER TABLE migration_session ADD COLUMN checklist text NOT NULL DEFAULT '';
