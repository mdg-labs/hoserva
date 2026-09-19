-- sqlite-migrate: checksum da81295dd22f06809c5b17aaf2832136f4277757fb1baee2446801a89c9102c8

ALTER TABLE schema_info ADD COLUMN hostname text;

ALTER TABLE schema_info ADD COLUMN timezone text;

ALTER TABLE schema_info ADD COLUMN backup_passphrase blob;
