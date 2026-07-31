CREATE TABLE IF NOT EXISTS audio_hashes (
    hash INTEGER,
    song_id TEXT,
    anchor_time REAL
);

CREATE INDEX IF NOT EXISTS idx_audio_hashes ON audio_hashes(hash);