-- Stores the core metadata maps as JSON objects to perfectly align with the Go struct.
CREATE TABLE IF NOT EXISTS songs (
    song_id TEXT PRIMARY KEY,
    canonical_composer JSON,  -- maps to map[string]string
    canonical_title JSON,     -- maps to map[string]string
    year JSON,                -- maps to map[string]int
    midi_filename JSON,       -- maps to map[string]string
    audio_filename JSON       -- maps to map[string]string
);

-- Tracks the global occurrence of a hash for TF-IDF scoring.
CREATE TABLE IF NOT EXISTS hash_weight (
    hash INTEGER PRIMARY KEY,
    track_count INTEGER DEFAULT 1,
    weight REAL
);

-- Stores the individual fingerprint points.
CREATE TABLE IF NOT EXISTS audio_hashes (
    hash INTEGER,
    song_id TEXT,
    anchor_time REAL,
    FOREIGN KEY (song_id) REFERENCES songs(song_id) ON DELETE CASCADE,
    FOREIGN KEY (hash) REFERENCES hash_weight(hash) ON DELETE CASCADE,
    UNIQUE(hash, song_id, anchor_time)
);

CREATE INDEX IF NOT EXISTS idx_audio_hashes_hash ON audio_hashes(hash);

CREATE INDEX IF NOT EXISTS idx_audio_hashes_song_id ON audio_hashes(song_id);