-- sqlite-migrate: checksum d4ad63fe4effcd92d36e8922782f5d9b17c9a35467ea2ac02ab3bbd9b7ba5026

ALTER TABLE shares ADD COLUMN min_free_space text NOT NULL DEFAULT '';

ALTER TABLE shares ADD COLUMN target_cache_mode text NOT NULL DEFAULT '' CHECK (target_cache_mode in ('', 'cache-then-move', 'cache-only', 'array-only'));

ALTER TABLE shares ADD COLUMN migration_notes text NOT NULL DEFAULT '[]';
