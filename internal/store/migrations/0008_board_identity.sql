-- Boards no longer split by track hash (docs/protocol.md, Time trials): a racing-line tweak in a mod changes the hash
-- but not the results, so every board identity keeps its lowest board id and takes the runs of its other boards.
-- Runs keep their own track_hash.
UPDATE runs r
    JOIN boards b ON b.id = r.board_id
    JOIN (
        SELECT track_id, engine_class, laps, content_version, MIN(id) AS keep_id
        FROM boards GROUP BY track_id, engine_class, laps, content_version
    ) k ON k.track_id = b.track_id AND k.engine_class = b.engine_class AND k.laps = b.laps
        AND k.content_version = b.content_version
SET r.board_id = k.keep_id
WHERE r.board_id <> k.keep_id;

DELETE b FROM boards b
    JOIN boards k ON k.track_id = b.track_id AND k.engine_class = b.engine_class AND k.laps = b.laps
        AND k.content_version = b.content_version AND k.id < b.id;

ALTER TABLE boards
    DROP KEY boards_identity,
    DROP COLUMN track_hash,
    ADD UNIQUE KEY boards_identity (track_id, engine_class, laps, content_version)
