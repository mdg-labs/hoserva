-- sqlite-migrate: checksum 6aabedb732cf92b2c89738588f4b8b4961d9f7d348d012917227945e9ab39bc5

ALTER TABLE schema_info ADD COLUMN catalog_refresh_interval text NOT NULL DEFAULT '24h' CHECK (catalog_refresh_interval in ('off', '1h', '6h', '12h', '24h'));

ALTER TABLE schema_info ADD COLUMN catalog_check_on_open integer NOT NULL DEFAULT 1 CHECK (catalog_check_on_open in (0, 1));
