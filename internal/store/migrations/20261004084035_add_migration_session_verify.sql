-- sqlite-migrate: checksum 582e13ffa01e52420c2e1a620ccbd6681a4a7812c7ce28e0e7b2816ae01d6c23

ALTER TABLE migration_session ADD COLUMN verify text NOT NULL DEFAULT '';
