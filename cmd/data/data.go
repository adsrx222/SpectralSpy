package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"spectralspy/data"
	"spectralspy/pkg/SpectralSpy"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/vmihailenco/msgpack/v5"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on system environment variables")
	}

	// prevent system sleep on macOS during execution
	caffeinate := exec.Command("caffeinate", "-dimsu")
	if err := caffeinate.Start(); err == nil {
		defer func() {
			if caffeinate.Process != nil {
				_ = caffeinate.Process.Kill()
			}
		}()
	}

	if err := data.PrepareMaestro(data.WorkspaceDir); err != nil {
		log.Fatalf("Error preparing Maestro dataset: %v", err)
	}

	songMap, err := data.LoadSongMap(data.WorkspaceDir)
	if err != nil {
		log.Fatalf("Error loading song metadata: %v", err)
	}

	fmt.Println("\n========================================")
	fmt.Println("       SPECTRALSPY DATA PIPELINE       ")
	fmt.Println("========================================")
	fmt.Println("1) Update Turso DB (Generate local SQLite & upload via Turso CLI)")
	fmt.Println("2) Update S3 MIDI Files")
	fmt.Println("3) Update S3 Constellation Msgpack Files")
	fmt.Print("\nSelect an option (1-3): ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return
	}
	choice := strings.TrimSpace(scanner.Text())

	switch choice {
	case "1":
		if err := UpdateTursoDB(data.WorkspaceDir, songMap); err != nil {
			log.Fatalf("❌ Turso DB update failed: %v", err)
		}
	case "2":
		if err := UpdateS3Midi(data.WorkspaceDir, songMap); err != nil {
			log.Fatalf("❌ S3 MIDI update failed: %v", err)
		}
	case "3":
		if err := UpdateS3Constellations(data.WorkspaceDir, songMap); err != nil {
			log.Fatalf("❌ S3 Constellations update failed: %v", err)
		}
	default:
		fmt.Println("Invalid choice. Exiting.")
	}
}

// update turso db via local sqlite + option to turso import
func UpdateTursoDB(workspaceDir string, songMap map[string]data.SongMetadata) error {
	sqlitePath := "misc/workspacehashes.sqlite"

	// remove old local database file if present
	_ = os.Remove(sqlitePath)

	log.Printf("📦 Creating local SQLite database at %s...", sqlitePath)
	localDB, err := sql.Open("sqlite3", sqlitePath)
	if err != nil {
		return fmt.Errorf("failed to open local sqlite db: %w", err)
	}

	// execute db/schema.sql to create schema
	schemaSQL, err := os.ReadFile("db/schema.sql")
	if err != nil {
		localDB.Close()
		return fmt.Errorf("failed to read db/schema.sql: %w", err)
	}
	if _, err := localDB.Exec(string(schemaSQL)); err != nil {
		localDB.Close()
		return fmt.Errorf("failed to execute schema.sql: %w", err)
	}
	log.Println("✅ Schema initialized from db/schema.sql")

	// process WAVs and batch insert into local SQLite
	err = data.ProcessWavFiles(workspaceDir, songMap, func(fileName string, meta data.SongMetadata, r io.Reader, current, total int) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		songID := data.GetSongID(meta.WavPath)
		log.Printf("[%d/%d] Processing hashes for: %s (ID: %s)", current, total, meta.Title, songID)

		samples, err := data.DecodeWavToFloat64(r)
		if err != nil {
			return fmt.Errorf("failed to decode wav: %w", err)
		}

		hashes, _ := SpectralSpy.ProcessWithPeaks(ctx, samples)
		if err := data.BatchInsertHashes(ctx, localDB, songID, hashes); err != nil {
			return fmt.Errorf("failed to insert hashes: %w", err)
		}
		log.Printf("  -> Inserted %d hashes into local DB", len(hashes))
		return nil
	})
	if err != nil {
		localDB.Close()
		return err
	}

	localDB.Close() // Ensure all connections and locks are released before CLI import

	tursoDBName := os.Getenv("DB_NAME")
	if tursoDBName == "" {
		tursoDBName = "spectralspy-db"
	}

	log.Printf("🚀 Preparing WAL and importing %s to Turso DB '%s' via Bash...", sqlitePath, tursoDBName)

	// execute SQLite PRAGMA commands and Turso import in a single bash shell context
	bashScript := fmt.Sprintf(`
		set -e
		sqlite3 %s "PRAGMA journal_mode=WAL; PRAGMA wal_checkpoint(TRUNCATE);"
		turso db import %s %s
	`, sqlitePath, sqlitePath, tursoDBName)

	cmd := exec.Command("bash", "-c", bashScript)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bash turso import command failed: %w", err)
	}

	log.Println("🎉 Turso DB successfully updated!")
	return nil
}

// fill/update S3 MIDI files
func UpdateS3Midi(workspaceDir string, songMap map[string]data.SongMetadata) error {
	r2Client, r2Bucket, err := data.InitCloudflareR2()
	if err != nil {
		return fmt.Errorf("failed to initialize R2 client: %w", err)
	}

	midiZipPath := fmt.Sprintf("%s/maestro-v3.0.0-midi.zip", workspaceDir)
	zr, err := data.OpenZipReader(midiZipPath)
	if err != nil {
		return fmt.Errorf("error opening midi zip: %w", err)
	}
	defer zr.Close()

	total := len(songMap)
	current := 1

	for _, meta := range songMap {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		songID := data.GetSongID(meta.WavPath)

		log.Printf("[%d/%d] Extracting MIDI for: %s (ID: %s)", current, total, meta.Title, songID)

		midiBytes, err := data.ExtractMidiToMemory(zr, meta.MidiPath)
		if err != nil {
			cancel()
			return fmt.Errorf("failed extracting midi %s: %w", meta.MidiPath, err)
		}

		s3Key := fmt.Sprintf("midi/%s.mid", songID)
		if err := data.UploadToR2(ctx, r2Client, r2Bucket, s3Key, midiBytes, "audio/midi"); err != nil {
			cancel()
			return fmt.Errorf("failed uploading midi to R2: %w", err)
		}

		log.Printf("  -> Uploaded %s to R2", s3Key)
		cancel()
		current++
	}

	log.Println("🎉 All MIDI files successfully uploaded to S3!")
	return nil
}

// fill/update S3 msgpack files
func UpdateS3Constellations(workspaceDir string, songMap map[string]data.SongMetadata) error {
	r2Client, r2Bucket, err := data.InitCloudflareR2()
	if err != nil {
		return fmt.Errorf("failed to initialize R2 client: %w", err)
	}

	return data.ProcessWavFiles(workspaceDir, songMap, func(fileName string, meta data.SongMetadata, r io.Reader, current, total int) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		songID := data.GetSongID(meta.WavPath)
		log.Printf("[%d/%d] Extracting Constellations for: %s (ID: %s)", current, total, meta.Title, songID)

		samples, err := data.DecodeWavToFloat64(r)
		if err != nil {
			return fmt.Errorf("failed decoding wav: %w", err)
		}

		_, peaks := SpectralSpy.ProcessWithPeaks(ctx, samples)
		peaksBytes, err := msgpack.Marshal(peaks)
		if err != nil {
			return fmt.Errorf("failed encoding peaks: %w", err)
		}

		s3Key := fmt.Sprintf("peaks/cm_%s.msgpack", songID)
		if err := data.UploadToR2(ctx, r2Client, r2Bucket, s3Key, peaksBytes, "application/msgpack"); err != nil {
			return fmt.Errorf("failed uploading constellations to R2: %w", err)
		}

		log.Printf("  -> Uploaded %s to R2", s3Key)
		return nil
	})
}