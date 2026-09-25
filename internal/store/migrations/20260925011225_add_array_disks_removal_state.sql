-- sqlite-migrate: checksum 850747a9766b969e62ec16a84e7ae97363a4a486609841684209aee3615e0fc3

ALTER TABLE array_disks ADD COLUMN removal_state text CHECK (removal_state is null or removal_state in ('evacuating', 'evacuated', 'unpooled', 'unlisted'));

ALTER TABLE array_disks ADD COLUMN removal_job_id text;
