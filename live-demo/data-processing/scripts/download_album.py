#!/usr/bin/env python3
"""
YouTube Album -> WAV -> SQLite Ingestion Pipeline
==================================================

Downloads every track from a YouTube playlist/album as a high-quality .wav
file using its original title, then upserts each song's (song_id, title, composer)
into a SQLite database initialized from a provided schema file.

song_id is computed as the SHA1 hash of the downloaded .wav file's *filename*
(not its contents), so it is deterministic and stable across re-runs as long
as the filename doesn't change.

Dependencies:
    pip install yt-dlp

External requirement:
    ffmpeg must be installed and on PATH (yt-dlp shells out to it for the
    WAV conversion postprocessor).

Usage:
    python youtube_album_ingest.py <workspace_dir> <album_url> <schema_path> <db_path>

Example:
    python live-demo/scripts/download_album.py \
    ./live-demo/workspace \
    "https://www.youtube.com/playlist?list=PL5A-FXHBxgj3Gfluaje-MuhXC5iBsZF5w" \
    ./live-demo/server/db/schema.sql \
    ./live-demo/workspace/db.sqlite
"""

from __future__ import annotations

import argparse
import hashlib
import os
import sqlite3
from pathlib import Path
from typing import Dict, List, Optional, Tuple

import yt_dlp

def init_database(db_path: str, schema_path: str) -> sqlite3.Connection:
    """Create/open the SQLite database at db_path and apply the schema file."""
    schema_file = Path(schema_path)
    if not schema_file.is_file():
        raise FileNotFoundError(f"Schema file not found: {schema_path}")

    schema_sql = schema_file.read_text(encoding="utf-8")

    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA foreign_keys = ON;")
    conn.executescript(schema_sql)
    conn.commit()

    print(f"[db] Initialized database at '{db_path}' using schema '{schema_path}'")
    return conn

def fetch_playlist_entries(album_url: str) -> List[Dict]:
    """
    Resolve full metadata for every video in the playlist/album up front.

    Uses non-flat extraction so each entry already contains rich fields
    (title, artist, track, uploader, webpage_url, id, ...) without a second
    network round-trip per video later.
    """
    ydl_opts = {
        "quiet": False,
        "skip_download": True,
        "ignoreerrors": True,  # skip private/deleted videos instead of aborting
        "cookiefile": "cookies.txt",  # Pass cookies to avoid bot detection on playlist load
    }

    with yt_dlp.YoutubeDL(ydl_opts) as ydl:
        info = ydl.extract_info(album_url, download=False)

    if info is None:
        raise RuntimeError(f"Could not resolve playlist/album: {album_url}")

    entries = info.get("entries")
    if entries is None:
        # A single video URL was passed instead of a playlist/album URL.
        entries = [info]

    valid_entries = [e for e in entries if e is not None]

    print(f"[playlist] Found {len(valid_entries)} track(s) in album")
    return valid_entries

def extract_composer_and_title(info: Dict) -> Tuple[str, str]:
    """
    Best-effort extraction of (composer, title) from yt-dlp video metadata.
    """
    artist = info.get("artist")
    track = info.get("track")
    raw_title = (info.get("title") or "Unknown Title").strip()

    if artist:
        composer = artist.strip()
        title = (track or raw_title).strip()
        return composer, title

    if " - " in raw_title:
        left, right = raw_title.split(" - ", 1)
        return left.strip(), right.strip()

    composer = (info.get("uploader") or info.get("channel") or "Unknown Composer").strip()
    return composer, raw_title

def _progress_hook(status: Dict) -> None:
    """yt-dlp progress hook: prints a compact per-download status line."""
    raw_filename = status.get("filename") or ""
    filename = os.path.basename(raw_filename)

    if status["status"] == "downloading":
        pct = status.get("_percent_str", "").strip()
        speed = status.get("_speed_str", "").strip()
        eta = status.get("_eta_str", "").strip()
        print(f"[download] {filename}: {pct} at {speed}, ETA {eta}", end="\r")
    elif status["status"] == "finished":
        print(f"\n[download] Finished downloading: {filename} (converting to wav...)")

def download_track_as_wav(video_url: str, destination_dir: str) -> Optional[str]:
    """
    Download the highest-quality audio for a single video, converted to .wav.

    Filenames are templated as "<title>.wav" using the video's original title,
    sanitized by yt-dlp for filesystem compatibility.

    Returns the absolute path to the resulting .wav file, or None on failure.
    """
    Path(destination_dir).mkdir(parents=True, exist_ok=True)

    # Saved using the original title instead of video ID
    outtmpl = os.path.join(destination_dir, "%(title)s.%(ext)s")

    ydl_opts = {
        "format": "bestaudio/best",
        "outtmpl": outtmpl,
        "postprocessors": [{
            "key": "FFmpegExtractAudio",
            "preferredcodec": "wav",
            "preferredquality": "0", 
        }],        
        "quiet": True,
        "no_warnings": True,
        "progress_hooks": [_progress_hook],
        "cookiefile": "cookies.txt", # Replaced "no_cookies": True with the exported cookie file
    }

    with yt_dlp.YoutubeDL(ydl_opts) as ydl:
        try:
            info = ydl.extract_info(video_url, download=True)
        except yt_dlp.utils.DownloadError as e:
            print(f"[download] ERROR downloading {video_url}: {e}")
            return None
 
        if info is None:
            print(f"[download] ERROR: no metadata returned for {video_url}")
            return None
 
        # FIX: Check requested_downloads for post-processed filename first
        wav_path = None
        if info.get("requested_downloads"):
            wav_path = info["requested_downloads"][0].get("filepath")
        
        if not wav_path:
            pre_pp_path = ydl.prepare_filename(info)
            wav_path = os.path.splitext(pre_pp_path)[0] + ".wav"
 
    if not os.path.isfile(wav_path):
        print(f"[download] WARNING: expected wav file not found at {wav_path}")
        return None
 
    return os.path.abspath(wav_path)

def compute_song_id(wav_path: str) -> str:
    """Derive song_id as the SHA1 hash of the .wav file's filename (not its bytes)."""
    filename = os.path.basename(wav_path)
    return hashlib.sha1(filename.encode("utf-8")).hexdigest()

def upsert_song(conn: sqlite3.Connection, song_id: str, title: str, composer: str) -> None:
    """Insert or update a song row in the SQLite database."""
    conn.execute(
        """
        INSERT INTO songs (song_id, title, composer)
        VALUES (?, ?, ?)
        ON CONFLICT(song_id) DO UPDATE SET
            title    = excluded.title,
            composer = excluded.composer
        """,
        (song_id, title, composer),
    )
    conn.commit()

def process_album(workspace_dir: str, album_url: str, schema_path: str, db_path: str) -> None:
    """Top-level pipeline: init db, resolve playlist, download + upsert each track."""
    waveforms_dir = os.path.join(workspace_dir, "waveforms")
    Path(waveforms_dir).mkdir(parents=True, exist_ok=True)

    conn = init_database(db_path, schema_path)

    try:
        entries = fetch_playlist_entries(album_url)
        total = len(entries)

        for index, entry in enumerate(entries, start=1):
            title_hint = entry.get("title", "Unknown")
            print(f"\n[{index}/{total}] Processing: {title_hint}")

            video_url = entry.get("webpage_url")
            if not video_url and entry.get("id"):
                video_url = f"https://www.youtube.com/watch?v={entry['id']}"
            if not video_url:
                print(f"[{index}/{total}] SKIPPED (no resolvable URL): {title_hint}")
                continue

            try:
                wav_path = download_track_as_wav(video_url, waveforms_dir)
                if wav_path is None:
                    print(f"[{index}/{total}] SKIPPED (download failed): {title_hint}")
                    continue

                composer, title = extract_composer_and_title(entry)
                song_id = compute_song_id(wav_path)

                upsert_song(conn, song_id, title, composer)

                print(
                    f"[{index}/{total}] DONE: '{title}' by {composer} "
                    f"(song_id={song_id[:10]}..., file={os.path.basename(wav_path)})"
                )
            except Exception as e:
                print(f"[{index}/{total}] ERROR processing '{title_hint}': {e}")
                continue

        print(f"\n[pipeline] Completed. Processed {total} track(s) into '{db_path}'.")

    finally:
        conn.close()

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Download a YouTube album/playlist as WAV files and index it into a SQLite database."
    )
    parser.add_argument("workspace_dir", help="Path to the workspace directory (WAVs saved to <workspace_dir>/waveforms)")
    parser.add_argument("album_url", help="YouTube playlist/album URL")
    parser.add_argument("schema_path", help="Path to the SQL schema file used to initialize the database")
    parser.add_argument("db_path", help="Path to the SQLite database file (created if missing)")

    args = parser.parse_args()

    process_album(
        workspace_dir=args.workspace_dir,
        album_url=args.album_url,
        schema_path=args.schema_path,
        db_path=args.db_path,
    )

if __name__ == "__main__":
    main()