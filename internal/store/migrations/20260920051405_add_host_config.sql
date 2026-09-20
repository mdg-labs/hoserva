-- sqlite-migrate: checksum 416a6fc9b6facc8907a8623b79d00b487dfdc63939f6d90a0c2461492ef733ff

CREATE TABLE host_config (
    kind TEXT PRIMARY KEY CHECK (kind IN ('samba', 'nfs', 'fstab', 'docker_containers', 'docker_images')),
    decision TEXT NOT NULL CHECK (decision IN ('import', 'leave')),
    facts TEXT NOT NULL,
    applied_at TEXT NOT NULL
) STRICT;
