-- sqlite-migrate: checksum ceb358e980af7ee692879fec4659075d35fbd16781ba420745946a83413e0e48

ALTER TABLE container_image_history ADD COLUMN snapshot_restored_at text NOT NULL DEFAULT '';
