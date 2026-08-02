package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
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

	_ "github.com/mattn/go-sqlite3"
)

type Trial struct {
	FreqBins    int     `json:"freq_bins"`
	TimeEnd     int     `json:"time_end"`
	Accuracy    float64 `json:"accuracy"`
	AvgQueryTime float64 `json:"avg_query_time"`
	HashCount   int     `json:"hash_count"`
	Fitness     float64 `json:"fitness"`
}

type OptResults struct {
	BestTrial Trial   `json:"best_trial"`
	Trials    []Trial `json:"trials"`
}

func rbf(r float64) float64 {
	return math.Sqrt(r*r + 1.0)
}

func distance(x1, y1, x2, y2 float64) float64 {
	dx := x1 - x2
	dy := y1 - y2
	return math.Sqrt(dx*dx + dy*dy)
}

func solveLinearSystem(A [][]float64, b []float64) ([]float64, error) {
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

func evaluateParams(ctx context.Context, workspaceDir string, corpus []testutil.SongMetadataInfo, queries []testutil.EvaluationQuery, freqBins, timeEnd int, w1, w2, w3 float64) (Trial, error) {
	dbPath := filepath.Join(workspaceDir, fmt.Sprintf("opt_temp_%d_%d.sqlite", freqBins, timeEnd))
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
		hashes, _ := SpectralSpy.ProcessWithParams(ctx, samples, freqBins, timeEnd)
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
		qHashes, _ := SpectralSpy.ProcessWithParams(ctx, q.Samples, freqBins, timeEnd)

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
	fitness := (w1 * accuracy) - (w2 * math.Log(float64(hashDensity))) - (w3 * avgTime)

	return Trial{
		FreqBins:     freqBins,
		TimeEnd:      timeEnd,
		Accuracy:     accuracy,
		AvgQueryTime: avgTime,
		HashCount:    totalHashes,
		Fitness:      fitness,
	}, nil
}

func main() {
	workspaceDir := flag.String("workspace", "misc/workspace", "Path to workspace directory")
	w1 := flag.Float64("w1", 100.0, "Weight for Accuracy (0.0 to 1.0 ratio)")
	w2 := flag.Float64("w2", 1.0, "Weight for log(HashDensity)")
	w3 := flag.Float64("w3", 0.1, "Weight for Average Query Time (ms)")
	pct := flag.Float64("pct", 0.02, "Stratified sampling percentage (default 0.02)")
	queryCount := flag.Int("queries", 50, "Number of evaluation queries")
	flag.Parse()

	fmt.Println("====================================================")
	fmt.Println("   BAYESIAN TARGET ZONE PARAMETER OPTIMIZER")
	fmt.Println("====================================================")
	fmt.Printf("Configured Weights: w1=%.2f, w2=%.2f, w3=%.2f\n", *w1, *w2, *w3)
	fmt.Printf("Sampling %.1f%% of tracks for Micro-Corpus...\n", *pct*100.0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	corpus, err := testutil.StratifiedSampleMaestro(*workspaceDir, *pct)
	if err != nil {
		fmt.Printf("Error generating stratified micro-corpus: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Successfully generated micro-corpus with %d tracks (including noise tracks).\n", len(corpus))

	queries, err := testutil.GenerateEvaluationQuerySet(*workspaceDir, corpus, *queryCount)
	if err != nil {
		fmt.Printf("Error generating evaluation query set: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Successfully generated %d distorted evaluation queries.\n", len(queries))

	// Parameter Space:
	// FREQ_BINS ∈ [2, 8]
	// TIME_END ∈ [3, 7]
	type Point struct {
		X, Y int
	}

	var allPoints []Point
	for fb := 2; fb <= 8; fb++ {
		for te := 3; te <= 7; te++ {
			allPoints = append(allPoints, Point{fb, te})
		}
	}

	initialPoints := []Point{
		{2, 3},
		{8, 7},
		{5, 5},
		{3, 6},
		{7, 4},
	}

	evaluated := make(map[Point]Trial)
	var trials []Trial

	evaluatePoint := func(p Point) (Trial, error) {
		if t, ok := evaluated[p]; ok {
			return t, nil
		}
		fmt.Printf("Evaluating candidate: FREQ_BINS=%d, TIME_END=%d ... ", p.X, p.Y)
		trial, err := evaluateParams(ctx, *workspaceDir, corpus, queries, p.X, p.Y, *w1, *w2, *w3)
		if err != nil {
			return Trial{}, err
		}
		fmt.Printf("Fitness=%.4f (Acc=%.2f%%, Time=%.2fms, Hashes=%d)\n",
			trial.Fitness, trial.Accuracy*100.0, trial.AvgQueryTime, trial.HashCount)
		evaluated[p] = trial
		trials = append(trials, trial)
		return trial, nil
	}

	// evaluate initial points
	for _, p := range initialPoints {
		if _, err := evaluatePoint(p); err != nil {
			fmt.Printf("Evaluation failed for point %+v: %v\n", p, err)
			os.Exit(1)
		}
	}

	// bayesian surrogate optimization loop
	kappa := 1.5 // Exploration parameter
	maxIterations := 10

	for iter := 1; iter <= maxIterations; iter++ {
		fmt.Printf("\n--- Surrogate Optimization Iteration %d/%d ---\n", iter, maxIterations)

		k := len(trials)
		A := make([][]float64, k)
		Y := make([]float64, k)
		for i := 0; i < k; i++ {
			A[i] = make([]float64, k)
			pI := Point{trials[i].FreqBins, trials[i].TimeEnd}
			Y[i] = trials[i].Fitness

			for j := 0; j < k; j++ {
				pJ := Point{trials[j].FreqBins, trials[j].TimeEnd}
				r := distance(float64(pI.X), float64(pI.Y), float64(pJ.X), float64(pJ.Y))
				A[i][j] = rbf(r)
			}
		}

		weights, err := solveLinearSystem(A, Y)
		if err != nil {
			fmt.Printf("Surrogate fitting singular: using greedy coordinate search. Error: %v\n", err)
		}

		var bestAcquisition float64 = -math.MaxFloat64
		var nextPoint Point
		nextPointFound := false

		// scan unevaluated grid points
		for _, p := range allPoints {
			if _, ok := evaluated[p]; ok {
				continue
			}

			// predict using surrogate model
			pred := 0.0
			minDist := math.MaxFloat64
			for i := 0; i < k; i++ {
				pI := Point{trials[i].FreqBins, trials[i].TimeEnd}
				dist := distance(float64(p.X), float64(p.Y), float64(pI.X), float64(pI.Y))
				if dist < minDist {
					minDist = dist
				}
				if weights != nil {
					pred += weights[i] * rbf(dist)
				} else {
					// Fallback to average of nearest evaluated points
					pred += trials[i].Fitness / float64(k)
				}
			}

			// uncertainty is proportional to distance to nearest evaluated point
			uncertainty := minDist
			acq := pred + kappa*uncertainty

			if acq > bestAcquisition {
				bestAcquisition = acq
				nextPoint = p
				nextPointFound = true
			}
		}

		if !nextPointFound {
			fmt.Println("All grid points evaluated.")
			break
		}

		if _, err := evaluatePoint(nextPoint); err != nil {
			fmt.Printf("Evaluation failed for surrogate chosen point %+v: %v\n", nextPoint, err)
			os.Exit(1)
		}
	}

	// find best overall trial
	var bestTrial Trial
	bestTrial.Fitness = -math.MaxFloat64
	for _, t := range trials {
		if t.Fitness > bestTrial.Fitness {
			bestTrial = t
		}
	}

	fmt.Println("\n====================================================")
	fmt.Println("           OPTIMIZATION SEARCH SUMMARY")
	fmt.Println("====================================================")
	fmt.Printf("Best Parameter Set Found:\n")
	fmt.Printf("  FREQ_BINS : %d\n", bestTrial.FreqBins)
	fmt.Printf("  TIME_END  : %d\n", bestTrial.TimeEnd)
	fmt.Printf("  Fitness   : %.4f\n", bestTrial.Fitness)
	fmt.Printf("  Accuracy  : %.2f%%\n", bestTrial.Accuracy*100.0)
	fmt.Printf("  Avg Query : %.2f ms\n", bestTrial.AvgQueryTime)
	fmt.Printf("  Hash Count: %d\n", bestTrial.HashCount)

	// Save results
	results := OptResults{
		BestTrial: bestTrial,
		Trials:    trials,
	}

	resultsPath := filepath.Join(*workspaceDir, "optimization_results.json")
	resBytes, err := json.MarshalIndent(results, "", "  ")
	if err == nil {
		_ = os.WriteFile(resultsPath, resBytes, 0644)
		fmt.Printf("Optimization results saved to %s\n", resultsPath)
	}

	fmt.Println("🎉 Optimization run completed successfully!")
}
