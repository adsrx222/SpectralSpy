#!/usr/bin/env python3
import argparse
import subprocess
import os
import sys
import sqlite3
import hashlib
import glob

"""
Example usage:
    python ./live-demo/scripts/process_fingerprints.py \
        --workspace "./live-demo/workspace" \
        --schema "live-demo/db/schema.sql" "db/schema.sql" \
        --db "./live-demo/workspace/db.sqlite" \
        --main-go "./cmd/data/main.go"
"""

def setup_workspace(workspace_dir):
    """
    Ensures the directory structure expected by data_processing.go exists.
    """
    waveforms_dir = os.path.join(workspace_dir, "waveforms")
    constellations_dir = os.path.join(workspace_dir, "constellations")
    
    os.makedirs(waveforms_dir, exist_ok=True)
    os.makedirs(constellations_dir, exist_ok=True)
    
    print(f"Workspace directories verified at: {workspace_dir}")

def run_go_processor(main_go_path, workspace_dir, schema_paths, db_path):
    """
    Executes the Go main file as a subprocess with all required arguments.
    """
    print(f"Starting Go processor ({main_go_path})...")
    print(f"  DB Path:   {db_path}")
    print(f"  Workspace: {workspace_dir}")
    print(f"  Schemas:   {', '.join(schema_paths)}")
    print("-" * 50)
    
    try:
        command = ["go", "run", main_go_path, db_path, workspace_dir] + schema_paths
        subprocess.run(command, check=True)
        print("-" * 50)
        print("Go processing finished successfully.")
    except subprocess.CalledProcessError as e:
        print(f"\nError running Go script. Exit code: {e.returncode}", file=sys.stderr)
        sys.exit(1)

def populate_songs_table(workspace_dir, db_path):
    """
    Scans the processed WAV files, replicates the Go SHA-1 song_id hashing,
    extracts metadata from the filename, and inserts it into the songs table.
    """
    print("Populating 'songs' table with file metadata...")
    waveforms_dir = os.path.join(workspace_dir, "waveforms")
    wav_files = glob.glob(os.path.join(waveforms_dir, "*.wav"))
    
    if not wav_files:
        print("No WAV files found. Skipping metadata extraction.")
        return

    try:
        conn = sqlite3.connect(db_path)
        cursor = conn.cursor()
        
        count = 0
        for path in wav_files:
            # Replicate the sha1 hash logic from Go's getSongID
            song_id = hashlib.sha1(path.encode('utf-8')).hexdigest()
            
            # Extract basic metadata from filename
            filename = os.path.basename(path)
            title = os.path.splitext(filename)[0]
            composer = "Unknown"
            
            # Simple assumption: "Composer - Title.wav" formatting
            if " - " in title:
                parts = title.split(" - ", 1)
                composer = parts[0].strip()
                title = parts[1].strip()
            
            # Insert or ignore to handle re-runs gracefully
            cursor.execute('''
                INSERT OR IGNORE INTO songs (song_id, title, composer) 
                VALUES (?, ?, ?)
            ''', (song_id, title, composer))
            
            if cursor.rowcount > 0:
                count += 1
                
        conn.commit()
        conn.close()
        print(f"Successfully inserted {count} entries into the 'songs' table.")
        print("-" * 50)
    except sqlite3.Error as e:
        print(f"SQLite error populating songs table: {e}", file=sys.stderr)
        print("Ensure the 'songs' table exists in one of your schema files.", file=sys.stderr)

def main():
    parser = argparse.ArgumentParser(
        description="Wrapper script to run Go WAV processing."
    )
    parser.add_argument(
        "-w", "--workspace", 
        required=True, 
        help="Path to the workspace directory"
    )
    parser.add_argument(
        "-s", "--schema", 
        nargs='+',
        required=True, 
        help="Path(s) to the SQL schema file(s) (e.g., db/schema.sql live-demo/db/schema.sql)"
    )
    parser.add_argument(
        "-d", "--db", 
        required=True, 
        help="Path to the SQLite database file"
    )
    parser.add_argument(
        "-m", "--main-go",
        default="main.go",
        help="Path to the Go main.go file (default: main.go)"
    )
    
    args = parser.parse_args()

    setup_workspace(args.workspace)
    
    run_go_processor(args.main_go, args.workspace, args.schema, args.db)
    
    populate_songs_table(args.workspace, args.db)

if __name__ == "__main__":
    main()