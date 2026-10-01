-- sqlite-migrate: checksum 17271322e65f816313a014948477468295aab271e5ca8033490b4a18d403da27

CREATE TABLE stacks (
    name TEXT PRIMARY KEY,
    template_source TEXT NOT NULL,
    template_id TEXT NOT NULL,
    template_revision TEXT NOT NULL,
    compose TEXT NOT NULL,
    env BLOB NOT NULL,
    installed_at TEXT NOT NULL
) STRICT;
