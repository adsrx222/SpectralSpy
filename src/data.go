package SpectralSpy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

type Model [2]string 
type Models []Model

func loadModel(modelPath string) (Models, error) {
	file, err := os.OpenFile(modelPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open/create file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// if file is newly created return an empty slice
	if len(data) == 0 {
		return Models{}, nil
	}

	var models Models
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("failed parsing model.json: %w", err)
	}

	return models, nil
}

func saveModel(modelPath string, models Models) error {
	data, err := json.MarshalIndent(models, "", " ") 
	if err != nil { 
		return fmt.Errorf("failed encoding models: %w", err)
	}

	if err := os.WriteFile(modelPath, data, 0644); err != nil { 
		return fmt.Errorf("failed writing model.json: %w", err)
	}

	return nil
}

func processFingerprints(ctx context.Context, db *sql.DB, songID string, fps []Fingerprint) error {
	if len(fps) == 0 {
		return nil // exit if len = 0
	}

	// start transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// insert into audio hash table
	hashStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO audio_hashes (hash, song_id, anchor_time) 
		VALUES (?, ?, ?)
		ON CONFLICT(hash, song_id, anchor_time) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("prepare audio_hashes statement: %w", err)
	}
	defer hashStmt.Close()

	// track unique hashes to maintain tf-idf weighting table
	uniqueFPs := make(map[uint64]struct{}, len(fps))

	for _, fp := range fps {
		if _, err := hashStmt.ExecContext(ctx, int64(fp.Hash), songID, fp.AnchorTime); err != nil {
			return fmt.Errorf("exec audio_hashes insert: %w", err)
		}
		uniqueFPs[fp.Hash] = struct{}{}
	}

	// update weight table
	weightStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO hash_weight (hash, track_count, weight)
		VALUES (?, 1, 1.0)
		ON CONFLICT(hash) DO UPDATE SET
			track_count = track_count + 1
	`)
	if err != nil {
		return fmt.Errorf("prepare hash_weight statement: %w", err)
	}
	defer weightStmt.Close()

	for hash := range uniqueFPs {
		if _, err := weightStmt.ExecContext(ctx, int64(hash)); err != nil {
			return fmt.Errorf("exec hash_weight update for hash %d: %w", hash, err)
		}
	}

	return tx.Commit()
}

func processConstellation(workspacePath, songID string, constellation [][]ConstellationPoint) error {
	constellationBytes, err := msgpack.Marshal(constellation)
	if err != nil {
		return fmt.Errorf("failed encoding peaks: %w", err)
	}

	constellation_path := filepath.Join(workspacePath, "constellations", fmt.Sprintf("%s.msgpack", songID))
	if err := os.WriteFile(constellation_path, constellationBytes, 0644); err != nil {
		return fmt.Errorf("failed writing constellation file: %w", err)
	}

	return nil
}

func processSamples(ctx context.Context, db *sql.DB, songID, workspacePath string, samples []float64) error {
	fps, constellation := ProcessWithPeaks(ctx, samples)
	
	err := processFingerprints(ctx, db, songID, fps)
	if err != nil {
		return fmt.Errorf("Processing fingerprint error: %w", err)
	}

	err = processConstellation(workspacePath, songID, constellation)
	if err != nil {
		return fmt.Errorf("Processing constellation error: %w", err)
	}

	return nil
}

func processModel(workspacePath, wavPath, songID string) error {
	modelPath := filepath.Join(workspacePath, "model.json")

	models, err := loadModel(modelPath)
	if err != nil {
		return err
	}

	models = append(models, Model{
		songID,
		wavPath,
	})

	return saveModel(modelPath, models)
}

func RunDP(ctx context.Context, db *sql.DB) error {
	if len(os.Args) < 4 {
		return fmt.Errorf("usage: <app> <db_path> <workspace_path> <schema_path>...")
	}

	workspacePath := os.Args[2]
    // FIX: Accept all remaining arguments as schema paths
	schemaPaths := os.Args[3:]

	waveformPath := filepath.Join(workspacePath, "waveforms") // location of .WAV files

	files, err := filepath.Glob(filepath.Join(waveformPath, "*.wav"))
	if err != nil {
		return err // determine legitimacy of source files
	}
    
	fmt.Printf("Found %d WAV files in workspace.\n", len(files))

    // FIX: Iterate over all provided schema paths
	for _, schemaPath := range schemaPaths {
		schemaBytes, err := os.ReadFile(schemaPath)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", schemaPath, err)
		}

		// Split the schema SQL by semicolons and execute each statement
		queries := strings.Split(string(schemaBytes), ";")
		for _, query := range queries {
			query = strings.TrimSpace(query)
			if query == "" {
				continue // Skip empty statements
			}
			if _, err := db.Exec(query); err != nil {
				return fmt.Errorf("failed to execute query %s in %s: %w", query, schemaPath, err)
			}
		}
        fmt.Printf("Successfully applied schema: %s\n", schemaPath)
	}

    total := len(files)
	// for each .wav file in input, produce db inserts, model update, and constellation save
	for i, path := range files {
        filename := filepath.Base(path)
        
        fmt.Printf("[%d/%d] Processing %s...\n", i+1, total, filename)

		songID := getSongID(path)

		samples, err := processWAV(path)
		if err != nil {
			log.Printf("Error processing %s: %v", path, err)
			continue
		}

		err = processSamples(ctx, db, songID, workspacePath, samples)
		if err != nil {
			log.Printf("Error post-processing %s: %v", path, err)
			continue
		}

		err = processModel(workspacePath, path, songID)
		if err != nil {
			log.Printf("Error model: %v", err)
			continue
		}
        
        fmt.Printf("[%d/%d] Successfully processed %s\n", i+1, total, filename)
	}

	return nil
}