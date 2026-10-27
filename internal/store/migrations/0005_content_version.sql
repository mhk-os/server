-- The simulation version is now called content version (docs/protocol.md Hello). Keys follow the renamed columns.
ALTER TABLE boards RENAME COLUMN sim_version TO content_version;

ALTER TABLE runs RENAME COLUMN sim_version TO content_version;

ALTER TABLE pack_manifests RENAME COLUMN sim_version TO content_version;

ALTER TABLE pack_votes RENAME COLUMN sim_version TO content_version;

ALTER TABLE content_strikes RENAME COLUMN sim_version TO content_version
