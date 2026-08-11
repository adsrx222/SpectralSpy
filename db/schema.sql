-- tracks  global occurrence of a hash for TF-IDF weighting
CREATE TABLE IF NOT EXISTS hash_weight (
    hash INTEGER PRIMARY KEY,
    track_count INTEGER DEFAULT 1,
    weight REAL
);

-- stores individual fingerprint points
CREATE TABLE IF NOT EXISTS audio_hashes (
    hash INTEGER,
    song_id TEXT,
    anchor_time REAL,
    FOREIGN KEY (hash) REFERENCES hash_weight(hash) ON DELETE CASCADE,
    UNIQUE(hash, song_id, anchor_time)
);

CREATE INDEX IF NOT EXISTS idx_audio_hashes_hash ON audio_hashes(hash);

CREATE INDEX IF NOT EXISTS idx_audio_hashes_song_id ON audio_hashes(song_id);