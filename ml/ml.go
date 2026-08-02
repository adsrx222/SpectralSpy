package ml

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"spectralspy/data"
	"spectralspy/db"
	"spectralspy/pkg/SpectralSpy"
	"spectralspy/server"
	"spectralspy/testutil"
)

type Trial struct {
	TimeStart    int     `json:"time_start"`
	TimeEnd      int     `json:"time_end"`
	FreqBins     int     `json:"freq_bins"`
	Accuracy     float64 `json:"accuracy"`
	AvgQueryTime float64 `json:"avg_query_time"`
	HashCount    int     `json:"hash_count"`
	Fitness      float64 `json:"fitness"`
}

type Params struct {
	NFrames   int
	NFreqs    int
	TimeStart int
	TimeEnd   int
	FreqBins  int
}

type OptResults struct {
	BestTrial Trial   `json:"best_trial"`
	Trials    []Trial `json:"trials"`
}

// radial basis function
func RBF(r float64) float64 {
	return math.Sqrt(r*r + 1.0)
}

func Distance(p1, p2 Params) float64 {
	d3 := float64(p1.TimeStart - p2.TimeStart)
	d4 := float64(p1.TimeEnd - p2.TimeEnd)
	d5 := float64(p1.FreqBins - p2.FreqBins)
	return math.Sqrt(d3*d3 + d4*d4 + d5*d5)
}

func SolveLinearSystem(A [][]float64, b []float64) ([]float64, error) {
	n := len(A)
	mat := make([][]float64, n)
	for i := range mat {
		mat[i] = make([]float64, n+1)
		copy(mat[i], A[i])
		mat[i][n] = b[i]
	}

	for i := 0; i < n; i++ {
		maxRow := i
		for r := i + 1; r < n; r++ {
			if math.Abs(mat[r][i]) > math.Abs(mat[maxRow][i]) {
				maxRow = r
			}
		}
		if math.Abs(mat[maxRow][i]) < 1e-9 {
			return nil, fmt.Errorf("singular matrix")
		}
		mat[i], mat[maxRow] = mat[maxRow], mat[i]

		for r := i + 1; r < n; r++ {
			factor := mat[r][i] / mat[i][i]
			for c := i; c <= n; c++ {
				mat[r][c] -= factor * mat[i][c]
			}
		}
	}

	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		sum := mat[i][n]
		for j := i + 1; j < n; j++ {
			sum -= mat[i][j] * x[j]
		}
		x[i] = sum / mat[i][i]
	}
	return x, nil
}

func EvaluateParams(ctx context.Context, workspaceDir string, corpus []testutil.SongMetadataInfo, queries []testutil.EvaluationQuery, p Params, w1, w2, w3 float64) (Trial, error) {
	dbPath := filepath.Join(workspaceDir, fmt.Sprintf("opt_temp_%d_%d_%d_%d_%d.sqlite", p.NFrames, p.NFreqs, p.TimeStart, p.TimeEnd, p.FreqBins))
	_ = os.Remove(dbPath)

	dbConn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return Trial{}, err
	}
	defer func() {
		dbConn.Close()
		_ = os.Remove(dbPath)
	}()

	_, _ = dbConn.Exec("PRAGMA journal_mode=MEMORY;")
	_, _ = dbConn.Exec("PRAGMA synchronous=OFF;")

	if err := db.InitSchema(dbConn); err != nil {
		return Trial{}, err
	}

	totalHashes := 0
	for _, track := range corpus {
		samples, err := testutil.GetAudioForTrack(workspaceDir, track)
		if err != nil {
			continue
		}
		hashes, _ := SpectralSpy.ProcessWithParams(ctx, samples, p.TimeStart, p.TimeEnd, p.FreqBins)
		totalHashes += len(hashes)

		if err := data.BatchInsertHashes(ctx, dbConn, track.SongID, hashes); err != nil {
			return Trial{}, err
		}
	}

	app := server.NewApp(dbConn, nil, "", nil)
	app.DisableRateLimit = true

	correctCount := 0
	totalQueryTime := 0.0

	for _, q := range queries {
		qHashes, _ := SpectralSpy.ProcessWithParams(ctx, q.Samples, p.TimeStart, p.TimeEnd, p.FreqBins)

		fingerprints := make([]server.Fingerprint, len(qHashes))
		for j, h := range qHashes {
			fingerprints[j] = server.Fingerprint{Hash: h.Hash, AnchorTime: h.AnchorTime}
		}

		payload, _ := json.Marshal(server.IdentifyRequest{Fingerprints: fingerprints})
		req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(payload))
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		t0 := time.Now()
		app.HandleIdentify(rr, req)
		queryDur := float64(time.Since(t0).Nanoseconds()) / 1e6

		if rr.Code == http.StatusOK {
			var resp server.IdentifyResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err == nil {
				if resp.SongID == q.ExpectedSongID {
					correctCount++
				}
			}
		}
		totalQueryTime += queryDur
	}

	accuracy := float64(correctCount) / float64(len(queries))
	avgTime := totalQueryTime / float64(len(queries))

	hashDensity := totalHashes
	if hashDensity < 1 {
		hashDensity = 1
	}
	
	//fitness := (w1 * accuracy) - (w2 * math.Log(float64(hashDensity))) - (w3 * avgTime)
	fitness := (w1 * accuracy) - w2*(avgTime/(math.Log(math.Max(1.0, float64(hashDensity)))+1e-6))

	return Trial{
		TimeStart:    p.TimeStart,
		TimeEnd:      p.TimeEnd,
		FreqBins:     p.FreqBins,
		Accuracy:     accuracy,
		AvgQueryTime: avgTime,
		HashCount:    totalHashes,
		Fitness:      fitness,
	}, nil
}