-- sqlite-migrate: checksum 2f51b24efe9abf9676b10664c24c32b04be4b325e5a260ac06246c3d20bd4154

CREATE UNIQUE INDEX users_username_lower_idx ON users (lower(username));
