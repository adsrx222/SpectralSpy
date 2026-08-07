package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adsrx222/SpectralSpy/SpectralSpy"
	"github.com/adsrx222/SpectralSpy/data"

	"github.com/hajimehoshi/go-mp3"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on system environment variables")
	}

	// Prevent system sleep on macOS during execution
	caffeinate := exec.Command("caffeinate", "-dimsu")
	if err := caffeinate.Start(); err == nil {
		defer func() {
			if caffeinate.Process != nil {
				_ = caffeinate.Process.Kill()
			}
		}()
	}

	fmt.Println("\n========================================")
	fmt.Println("   SPECTRALSPY LOCAL TESTDATA PIPELINE  ")
	fmt.Println("========================================")

	if err := ProcessTestData(); err != nil {
		log.Fatalf("❌ Pipeline failed: %v", err)
	}

	log.Println("🎉 Local testdata pipeline completed successfully!")
}

// ProcessTestData handles hashing, snippet generation, weight calculation, and Turso import
func ProcessTestData() error {
	inputDir := "./misc/testdata"
	dbDir := "./misc/workspace"
	snippetDir := "./misc/workspace/testdata"

	// 1. Ensure workspace directories exist
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return fmt.Errorf("failed to create db workspace dir: %w", err)
	}
	if err := os.MkdirAll(snippetDir, 0755); err != nil {
		return fmt.Errorf("failed to create snippet workspace dir: %w", err)
	}

	sqlitePath := filepath.Join(dbDir, "workspace_hash.sqlite")

	// Remove old local database file if present
	_ = os.Remove(sqlitePath)

	log.Printf("📦 Creating local SQLite database at %s...", sqlitePath)
	localDB, err := sql.Open("sqlite3", sqlitePath)
	if err != nil {
		return fmt.Errorf("failed to open local sqlite db: %w", err)
	}

	// Initialize schema
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

	// 2. Glob for MP3 files in input directory
	mp3Files, err := filepath.Glob(filepath.Join(inputDir, "*.mp3"))
	if err != nil {
		localDB.Close()
		return fmt.Errorf("failed to scan for mp3 files: %w", err)
	}
	if len(mp3Files) == 0 {
		localDB.Close()
		return fmt.Errorf("no mp3 files found in %s", inputDir)
	}

	rand.Seed(time.Now().UnixNano())

	// 3. Process each MP3 file
	for i, mp3Path := range mp3Files {
		fileName := filepath.Base(mp3Path)
		songID := strings.TrimSuffix(fileName, filepath.Ext(fileName)) // e.g., "001"

		log.Printf("[%d/%d] Processing %s (ID: %s)", i+1, len(mp3Files), fileName, songID)

		// --- A. Generate Hashes for SpectralSpy ---
		if err := hashAudio(localDB, mp3Path, songID); err != nil {
			log.Printf("⚠️ Failed to hash %s: %v", fileName, err)
			continue
		}

		// --- B. Generate 2-Second Random Snippet ---
		outSnippetPath := filepath.Join(snippetDir, fmt.Sprintf("%s_snippet.mp3", songID))
		if err := generateRandomSnippet(mp3Path, outSnippetPath, 2); err != nil {
			log.Printf("⚠️ Failed to generate snippet for %s: %v", fileName, err)
			continue
		}
	}

	// 4. Recalculate weights after all hashes are inserted
	log.Println("⚖️ Recalculating BM25 hash weights...")
	if err := data.RecalculateBM25Weights(context.Background(), localDB); err != nil {
		localDB.Close()
		return fmt.Errorf("failed to recalculate BM25 weights: %w", err)
	}
	localDB.Close()

	log.Println("🎉 DB successfully updated!")
	return nil
}

// hashAudio decodes the file, processes it via SpectralSpy, and inserts hashes into the DB
func hashAudio(db *sql.DB, filePath, songID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	samples, err := decodeMp3ToFloat64(file)
	if err != nil {
		return fmt.Errorf("failed to decode mp3: %w", err)
	}

	hashes, _ := SpectralSpy.ProcessWithPeaks(ctx, samples)
	if err := data.BatchInsertHashes(ctx, db, songID, hashes); err != nil {
		return fmt.Errorf("failed to insert hashes: %w", err)
	}

	log.Printf("  -> Inserted %d hashes into local DB", len(hashes))
	return nil
}

// decodeMp3ToFloat64 reads an MP3 stream, verifies its sample rate matches SpectralSpy.SAMPLE_RATE,
// downmixes it to mono, and normalizes it to float64
func decodeMp3ToFloat64(r io.Reader) ([]float64, error) {
	decoder, err := mp3.NewDecoder(r)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize mp3 decoder: %w", err)
	}

	if decoder.SampleRate() != SpectralSpy.SAMPLE_RATE {
		return nil, fmt.Errorf("audio sample rate %d Hz does not match target SpectralSpy.SAMPLE_RATE (%d Hz)", decoder.SampleRate(), SpectralSpy.SAMPLE_RATE)
	}

	var floats []float64
	buf := make([]byte, 8192)

	for {
		n, err := decoder.Read(buf)
		if n > 0 {
			for i := 0; i < n; i += 4 {
				if i+3 >= n {
					break
				}

				leftSample := int16(buf[i]) | (int16(buf[i+1]) << 8)
				rightSample := int16(buf[i+2]) | (int16(buf[i+3]) << 8)

				monoSample := (float64(leftSample) + float64(rightSample)) / 2.0
				floats = append(floats, monoSample/32768.0)
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading mp3 stream: %w", err)
		}
	}

	return floats, nil
}

// generateRandomSnippet uses ffprobe and ffmpeg to extract a clip of duration `durationSecs` resampled to SpectralSpy.SAMPLE_RATE
func generateRandomSnippet(inputPath, outputPath string, durationSecs int) error {
	durationCmd := exec.Command("ffprobe", "-v", "error", "-show_entries",
		"format=duration", "-of", "default=noprint_wrappers=1:nokey=1", inputPath)

	var out bytes.Buffer
	durationCmd.Stdout = &out
	if err := durationCmd.Run(); err != nil {
		return fmt.Errorf("ffprobe failed (is ffmpeg installed?): %w", err)
	}

	totalDuration, err := strconv.ParseFloat(strings.TrimSpace(out.String()), 64)
	if err != nil {
		return fmt.Errorf("failed to parse audio duration: %w", err)
	}

	maxStart := totalDuration - float64(durationSecs)
	if maxStart < 0 {
		maxStart = 0
	}
	randomStart := rand.Float64() * maxStart

	log.Printf("  -> Extracting %ds snippet starting at %.2fs to %s (Sample Rate: %d Hz)", durationSecs, randomStart, outputPath, SpectralSpy.SAMPLE_RATE)
	ffmpegCmd := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.2f", randomStart),
		"-t", strconv.Itoa(durationSecs),
		"-i", inputPath,
		"-ar", strconv.Itoa(SpectralSpy.SAMPLE_RATE),
		outputPath,
	)

	if err := ffmpegCmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg slicing failed: %w", err)
	}

	return nil
}