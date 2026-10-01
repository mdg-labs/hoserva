-- sqlite-migrate: checksum 757fcd3600607f1ce779c99c2e89cea864156343b3048366ac8c4a0e9b01563e

ALTER TABLE stacks ADD COLUMN manually_edited integer NOT NULL DEFAULT 0 CHECK (manually_edited in (0, 1));
