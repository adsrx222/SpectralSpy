package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	SpectralSpy "spectralspy/src-old"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/joho/godotenv"
	_ "github.com/tursodatabase/go-libsql"
	// "github.com/vmihailenco/msgpack/v5"
	"github.com/zeebo/xxh3"
)

const maestroURL = "https://storage.googleapis.com/magentadata/datasets/maestro/v3.0.0/maestro-v3.0.0.zip"
const maestroMIDI_URL = "https://storage.googleapis.com/magentadata/datasets/maestro/v3.0.0/maestro-v3.0.0-midi.zip"
const maestroJSON_URL = "https://storage.googleapis.com/magentadata/datasets/maestro/v3.0.0/maestro-v3.0.0.json"

type SongMetadata struct {
	Artist   string
	Title    string
	Year     int
	MidiPath string
	WavPath  string
}

type MaestroDataframe struct {
	CanonicalComposer map[string]string `json:"canonical_composer"`
	CanonicalTitle    map[string]string `json:"canonical_title"`
	Year              map[string]int    `json:"year"`
	MidiFilename      map[string]string `json:"midi_filename"`
	AudioFilename     map[string]string `json:"audio_filename"`
}

type WavProcessFunc func(fileName string, songMap map[string]SongMetadata, r io.Reader, current int, total int) error

func main() {
	err := godotenv.Load()
    if err != nil {
        log.Println("No .env file found, relying on system environment variables")
    }

	caffeinate := exec.Command("caffeinate", "-dimsu")
	if err := caffeinate.Start(); err == nil {
		defer func() {
			if caffeinate.Process != nil {
				_ = caffeinate.Process.Kill()
			}
		}()
	}
	
	workspaceDir := "artifacts/workspace"

	if err := PrepareMaestro(workspaceDir); err != nil {
		fmt.Fprintln(os.Stderr, "error preparing maestro:", err)
		os.Exit(1)
	}

	// 1. Initialize Turso Database
	dbUrl := os.Getenv("DB_URL")
	if dbUrl == "" {
		dbUrl = "file:hashes.sqlite" // Fallback to local SQLite if no Turso URL provided
	}

	dbTok := os.Getenv("DB_TOKEN")
	if dbTok == "" {
		// Token remains empty (valid for local fallback databases)
	}

	db, err := initTursoDB(dbUrl, dbTok)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error initializing turso db:", err)
		os.Exit(1)
	}
	defer db.Close()

	// 2. Initialize Cloudflare R2 Client
	r2Client, r2Bucket, err := initCloudflareR2()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error initializing r2:", err)
		os.Exit(1)
	}

	// Open the MIDI zip once
	midiZipPath := filepath.Join(workspaceDir, "maestro-v3.0.0-midi.zip")
	zr, err := zip.OpenReader(midiZipPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error opening midi zip:", err)
		os.Exit(1)
	}
	defer zr.Close()

	// Initialize the processing callback closure
	processor := createPipelineCallback(db, r2Client, r2Bucket, zr)

	if err := processWavFiles(workspaceDir, processor); err != nil {
		fmt.Fprintln(os.Stderr, "pipeline error:", err)
		os.Exit(1)
	}
}

// initTursoDB connects to Turso and automates the creation of the schema.
func initTursoDB(dbUrl, dbAuthToken string) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s?authToken=%s", dbUrl, dbAuthToken)

	db, err := sql.Open("libsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	queries := []string{
		`CREATE TABLE IF NOT EXISTS audio_hashes (
			hash INTEGER,
			song_id TEXT,
			anchor_time REAL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_audio_hashes ON audio_hashes(hash);`,
	}

	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			db.Close() // Clean up connection if setup fails
			return nil, fmt.Errorf("failed to execute schema setup: %w", err)
		}
	}

	return db, nil
}

// initCloudflareR2 sets up the AWS SDK client specifically for Cloudflare's R2 endpoints.
func initCloudflareR2() (*s3.Client, string, error) {
	accountID := os.Getenv("R2_ACCOUNT_ID")
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secretKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	bucketName := os.Getenv("R2_BUCKET_NAME")

	if accountID == "" || bucketName == "" {
		return nil, "", fmt.Errorf("missing R2 environment variables")
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, "", err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID))
	})

	return client, bucketName, nil
}

// createPipelineCallback builds the WavProcessFunc wrapping DB and R2.
// Notice the addition of `current int, total int` to the returned function
func createPipelineCallback(db *sql.DB, r2Client *s3.Client, r2Bucket string, zr *zip.ReadCloser) WavProcessFunc {
	return func(fileName string, songMap map[string]SongMetadata, r io.Reader, current int, total int) error {
		ctx := context.Background()

		meta, exists := songMap[fileName]
		if !exists {
			return fmt.Errorf("metadata not found for filename: %s", fileName)
		}

		hash64 := xxh3.HashString(meta.WavPath)
		songID := strconv.FormatUint(hash64, 36)

		// --- INJECT PROGRESS INTO THE LOG HEADER ---
		log.Printf("\n--- [%d / %d] Starting: %s (ID: %s) ---", current, total, meta.Title, songID)

		// Decode WAV
		samples, err := decodeWavToFloat64(r)
		if err != nil {
			return fmt.Errorf("failed to decode wav %s: %w", fileName, err)
		}

		// 1. Process Hashes and upload to Turso
		hashes, _ := SpectralSpy.ProcessWithPeaks(ctx, samples) // change to peaks after uncommenting
		if err := batchInsertHashes(ctx, db, songID, hashes); err != nil {
			return fmt.Errorf("failed to insert hashes to turso: %w", err)
		}
		log.Printf("[%s] ✔ HASHES uploaded to Database (%d records)", songID, len(hashes))

		// // 2. Export Peaks and upload to R2 
		// peaksBytes, err := msgpack.Marshal(peaks)
		// if err != nil {
		// 	return fmt.Errorf("failed to encode peaks: %w", err)
		// }
		// err = uploadToR2(ctx, r2Client, r2Bucket, fmt.Sprintf("peaks/cm_%s.msgpack", songID), peaksBytes, "application/msgpack")
		// if err != nil {
		// 	return fmt.Errorf("failed uploading peaks to r2: %w", err)
		// }
		// log.Printf("[%s] ✔ CONSTELLATION MAP uploaded to Cloudflare R2", songID)

		// // 3. Extract MIDI directly into memory and upload to R2
		// midiBytes, err := extractMidiToMemory(zr, meta.MidiPath)
		// if err != nil {
		// 	return fmt.Errorf("failed extracting midi %s: %w", meta.MidiPath, err)
		// }
		// err = uploadToR2(ctx, r2Client, r2Bucket, fmt.Sprintf("midi/%s.mid", songID), midiBytes, "audio/midi")
		// if err != nil {
		// 	return fmt.Errorf("failed uploading midi to r2: %w", err)
		// }
		// log.Printf("[%s] ✔ MIDI FILE uploaded to Cloudflare R2", songID)

		return nil
	}
}

// batchInsertHashes efficiently bulk-inserts thousands of hashes into Turso inside a transaction.
func batchInsertHashes(ctx context.Context, db *sql.DB, songID string, hashes []SpectralSpy.HashEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Use a prepared statement within the transaction for speed
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, h := range hashes {
		// Cast h.Hash (uint64) to int64 so the SQL driver accepts it without panicking on the high bit
		_, err = stmt.ExecContext(ctx, int64(h.Hash), songID, h.AnchorTime)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// uploadToR2 streams a byte array directly to Cloudflare R2
func uploadToR2(ctx context.Context, client *s3.Client, bucket, key string, data []byte, contentType string) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	return err
}

// extractMidiToMemory finds the targeted MIDI in the zip directory and returns its raw bytes.
func extractMidiToMemory(zr *zip.ReadCloser, targetMidiPath string) ([]byte, error) {
	for _, f := range zr.File {
		// Use strings.HasSuffix instead of == to ignore root folders like "maestro-v3.0.0/"
		if strings.HasSuffix(f.Name, targetMidiPath) {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("opening zip file entry: %w", err)
			}
			defer rc.Close()
			return io.ReadAll(rc) // Returns the file purely in memory
		}
	}
	return nil, fmt.Errorf("midi file %s not found in archive", targetMidiPath)
}

// processWavFiles, decodeWavToFloat64, PrepareMaestro, determineMissing, downloadFile remain the same...
// (Please include the previous versions of these functions below here)
func processWavFiles(workspaceDir string, process WavProcessFunc) error {
	// 1. Read the JSON file
	jsonPath := filepath.Join(workspaceDir, "maestro-v3.0.0.json")
	file, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("reading json: %w", err)
	}

	// 2. Unmarshal into the dataframe
	var df MaestroDataframe
	if err := json.Unmarshal(file, &df); err != nil {
		return fmt.Errorf("unmarshaling json dataframe: %w", err)
	}

	// 3. Create and populate the songMap
	songMap := make(map[string]SongMetadata)
	for key, audioPath := range df.AudioFilename {
		songMap[audioPath] = SongMetadata{
			Artist:   df.CanonicalComposer[key],
			Title:    df.CanonicalTitle[key],
			Year:     df.Year[key],
			MidiPath: df.MidiFilename[key],
			WavPath:  audioPath,
		}
	}

	// 4. Open the ZIP archive
	audioZipPath := filepath.Join(workspaceDir, "maestro-v3.0.0.zip")
	log.Printf("Opening main audio archive: %s\n", audioZipPath)
	zr, err := zip.OpenReader(audioZipPath)
	if err != nil {
		return fmt.Errorf("opening audio zip: %w", err)
	}
	defer zr.Close()

	// --- PROGRESS TRACKING VARIABLES ---
	totalSongs := len(songMap)
	currentSong := 1

	// 5. Loop through the ZIP
	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Ext(f.Name), ".wav") {
			continue
		}

		var matchedKey string
		for key := range songMap {
			if strings.HasSuffix(f.Name, key) {
				matchedKey = key
				break
			}
		}

		if matchedKey == "" {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening wav %s from zip: %w", f.Name, err)
		}

		// Pass currentSong and totalSongs into the callback
		if err := process(matchedKey, songMap, rc, currentSong, totalSongs); err != nil {
			rc.Close()
			return fmt.Errorf("processing %s: %w", f.Name, err)
		}
		rc.Close()

		// Increment the counter after a successful process
		currentSong++
	}

	return nil
}

func decodeWavToFloat64(r io.Reader) ([]float64, error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("failed to buffer stream for seeker: %w", err)
		}
		rs = bytes.NewReader(b)
	}

	d := wav.NewDecoder(rs)
	if !d.IsValidFile() {
		return nil, fmt.Errorf("invalid WAV stream")
	}

	format := d.Format()
	if format == nil {
		return nil, fmt.Errorf("could not parse WAV format")
	}

	numChannels := format.NumChannels
	bitDepth := d.BitDepth

	var maxVal float64
	switch bitDepth {
	case 8:
		maxVal = 128.0
	case 16:
		maxVal = 32768.0
	case 24:
		maxVal = 8388608.0
	case 32:
		maxVal = 2147483648.0
	default:
		return nil, fmt.Errorf("unsupported bit depth: %d", bitDepth)
	}

	var samples []float64
	buf := &audio.IntBuffer{
		Format:         format,
		Data:           make([]int, 8192*numChannels),
		SourceBitDepth: int(bitDepth),
	}

	for {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			return nil, fmt.Errorf("failed to read PCM buffer: %w", err)
		}
		if n == 0 {
			break
		}
		for i := 0; i < n; i += numChannels {
			var sum float64
			for c := 0; c < numChannels; c++ {
				sum += float64(buf.Data[i+c]) / maxVal
			}
			samples = append(samples, sum/float64(numChannels))
		}
	}
	return samples, nil
}

func PrepareMaestro(dir string) (err error) {
	workspaceStatus := determineMissing(dir)

	if !workspaceStatus[0] {
		if err = downloadFile(maestroURL, filepath.Join(dir, "maestro-v3.0.0.zip")); err != nil {
			return fmt.Errorf("failed to download maestro zip: %w", err)
		}
	}
	if !workspaceStatus[1] {
		if err = downloadFile(maestroJSON_URL, filepath.Join(dir, "maestro-v3.0.0.json")); err != nil {
			return fmt.Errorf("failed to download maestro json: %w", err)
		}
	}
	if !workspaceStatus[2] {
		if err = downloadFile(maestroMIDI_URL, filepath.Join(dir, "maestro-v3.0.0-midi.zip")); err != nil {
			return fmt.Errorf("failed to download maestro midi: %w", err)
		}
	}
	return nil
}

func determineMissing(workspaceDir string) []bool {
	results := make([]bool, 3)
	zipPath := filepath.Join(workspaceDir, "maestro-v3.0.0.zip")
	jsonPath := filepath.Join(workspaceDir, "maestro-v3.0.0.json")
	midiPath := filepath.Join(workspaceDir, "maestro-v3.0.0-midi.zip")

	if _, err := os.Stat(zipPath); err == nil {
		results[0] = true
	}
	if _, err := os.Stat(jsonPath); err == nil {
		results[1] = true
	}
	if _, err := os.Stat(midiPath); err == nil {
		results[2] = true
	}
	return results
}

func downloadFile(url, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	cmd := exec.Command("curl", "-L", "--fail", "-o", path, url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("curl download failed: %w", err)
	}
	return nil
}
