package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/time/rate"
)

// -----------------------------------------------------------------------------
// Models
// -----------------------------------------------------------------------------

type Fingerprint struct {
	Hash       uint64  `json:"hash"`
	AnchorTime float64 `json:"anchor_time"`
}

type IdentifyRequest struct {
	Fingerprints []Fingerprint `json:"fingerprints"`
}

type IdentifyResponse struct {
	SongID     string  `json:"song_id"`
	Confidence float64 `json:"confidence"`
	TimeOffset float64 `json:"time_offset"`
}

type APIError struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// OffsetKey groups matching hashes by their relative time distance 
type OffsetKey struct {
	SongID     string
	AnchorTime float64
}

// -----------------------------------------------------------------------------
// App State & Dependency Injection
// -----------------------------------------------------------------------------

type App struct {
	DB       *sql.DB
	S3       *s3.Client
	Bucket   string
	Logger   *slog.Logger
	Limiters *IPRateLimiter
}

// IPRateLimiter provides thread-safe, per-IP rate limiting (token bucket).
// Future migration: This interface/struct can easily be swapped for a Redis-backed implementation.
type IPRateLimiter struct {
	ips map[string]*rate.Limiter
	mu  *sync.RWMutex
	r   rate.Limit
	b   int
}

func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter {
	return &IPRateLimiter{
		ips: make(map[string]*rate.Limiter),
		mu:  &sync.RWMutex{},
		r:   r,
		b:   b,
	}
}

func (i *IPRateLimiter) GetLimiter(ip string) *rate.Limiter {
	i.mu.Lock()
	defer i.mu.Unlock()

	limiter, exists := i.ips[ip]
	if !exists {
		limiter = rate.NewLimiter(i.r, i.b)
		i.ips[ip] = limiter
	}
	return limiter
}

// -----------------------------------------------------------------------------
// Main Entrypoint
// -----------------------------------------------------------------------------

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// 1. Initialize SQLite Database
	dbUrl := os.Getenv("DB_URL")
	if dbUrl == "" {
		dbUrl = "hashes.sqlite"
	}
	db, err := sql.Open("sqlite", dbUrl)
	if err != nil {
		logger.Error("Failed to open database", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	// 2. Initialize Cloudflare R2 Client (AWS SDK v2)
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			os.Getenv("R2_ACCESS_KEY_ID"),
			os.Getenv("R2_SECRET_ACCESS_KEY"),
			"",
		)),
	)
	if err != nil {
		logger.Error("Failed to load AWS config", "err", err)
		os.Exit(1)
	}

	r2Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", os.Getenv("R2_ACCOUNT_ID")))
	})

	// 3. Initialize App State
	app := &App{
		DB:       db,
		S3:       r2Client,
		Bucket:   os.Getenv("R2_BUCKET_NAME"),
		Logger:   logger,
		Limiters: NewIPRateLimiter(rate.Limit(5), 10), // 5 req/sec, burst of 10
	}

	// 4. Configure Router
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(app.LoggerMiddleware)
	r.Use(app.RateLimitMiddleware)
	r.Use(middleware.Recoverer)

	// API Routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/identify", app.HandleIdentify)
		r.Get("/songs/{id}/midi", app.HandleGetMIDI)
		r.Get("/songs/{id}/constellation", app.HandleGetConstellation)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logger.Info("Starting server", "port", port)
	if err := http.ListenAndServe(":"+port, r); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("Server failed", "err", err)
	}
}

// -----------------------------------------------------------------------------
// Middlewares
// -----------------------------------------------------------------------------

func (a *App) LoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		
		next.ServeHTTP(ww, r)

		a.Logger.Info("HTTP Request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", r.RemoteAddr,
		)
	})
}

func (a *App) RateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		
		limiter := a.Limiters.GetLimiter(ip)
		if !limiter.Allow() {
			a.respondError(w, http.StatusTooManyRequests, "Rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// POST /identify
// POST /identify
func (a *App) HandleIdentify(w http.ResponseWriter, r *http.Request) {
	// 1. Limit body size to prevent DoS attacks
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024)

	var req IdentifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.respondError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}

	// 2. Validate fingerprint count
	if len(req.Fingerprints) == 0 {
		a.respondError(w, http.StatusBadRequest, "No fingerprints provided")
		return
	}
	if len(req.Fingerprints) > 500 {
		a.respondError(w, http.StatusBadRequest, "Maximum 500 fingerprints allowed per request")
		return
	}

	// Map query hashes to their anchor times for O(1) lookup during histogram generation
	// A slice is used as values because duplicate hashes can occasionally occur in a track
	queryMap := make(map[uint64][]float64)
	placeholders := make([]string, len(req.Fingerprints))
	args := make([]interface{}, len(req.Fingerprints))
	
	for i, fp := range req.Fingerprints {
		queryMap[fp.Hash] = append(queryMap[fp.Hash], fp.AnchorTime)
		placeholders[i] = "?"
		args[i] = int64(fp.Hash) // SQLite handles uint64 safely if cast to int64
	}

	// 3. Search SQLite database
	query := fmt.Sprintf(`
		SELECT hash, song_id, anchor_time 
		FROM audio_hashes 
		WHERE hash IN (%s)`, strings.Join(placeholders, ","))

	rows, err := a.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		a.Logger.Error("Database query failed", "err", err)
		a.respondError(w, http.StatusInternalServerError, "Internal database error")
		return
	}
	defer rows.Close()

	// 4. Histogram Matching Algorithm (Ported from SpectralSpy)
	const HOP_SIZE = 2048.0 // Derived from WINDOW_SIZE (4096) / 2
	globalHist := make(map[OffsetKey]int)

	for rows.Next() {
		var hash int64
		var songID string
		var dbAnchorTime float64
		
		if err := rows.Scan(&hash, &songID, &dbAnchorTime); err != nil {
			continue
		}
		
		// For every time this hash appeared in our query, calculate the relative offset
		if queryAnchors, exists := queryMap[uint64(hash)]; exists {
			for _, qAnchorTime := range queryAnchors {
				quantised := math.Round((dbAnchorTime - qAnchorTime) / HOP_SIZE)
				key := OffsetKey{SongID: songID, AnchorTime: quantised}
				globalHist[key]++
			}
		}
	}

	// 5. Calculate Confidence and Best Match
	var bestSong string
	var firstBestVotes int
	var secondBestVotes int
	var bestOffset float64

	for key, count := range globalHist {
		if count > firstBestVotes {
			secondBestVotes = firstBestVotes
			firstBestVotes = count
			bestSong = key.SongID
			bestOffset = key.AnchorTime
		} else if count > secondBestVotes {
			secondBestVotes = count
		}
	}

	if bestSong == "" {
		a.respondError(w, http.StatusNotFound, "No matching song found")
		return
	}

	// Protect against division by zero if there are no competing matches
	var conf float64
	if secondBestVotes == 0 {
		conf = float64(firstBestVotes) 
	} else {
		conf = float64(firstBestVotes) / float64(secondBestVotes)
	}

	// Return successful metadata
	a.respondJSON(w, http.StatusOK, IdentifyResponse{
		SongID:     bestSong,
		Confidence: conf,
		TimeOffset: bestOffset, 
	})
}

// GET /songs/{id}/midi
func (a *App) HandleGetMIDI(w http.ResponseWriter, r *http.Request) {
	songID := chi.URLParam(r, "id")
	if songID == "" {
		a.respondError(w, http.StatusBadRequest, "Missing song ID")
		return
	}

	objectKey := fmt.Sprintf("midi/%s.mid", songID)

	out, err := a.S3.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(a.Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		a.Logger.Warn("MIDI file not found in R2", "song_id", songID, "err", err)
		a.respondError(w, http.StatusNotFound, "MIDI file not found")
		return
	}
	defer out.Body.Close()

	// Set headers for browser streaming and downloading
	w.Header().Set("Content-Type", "audio/midi")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.mid"`, songID))
	w.Header().Set("Cache-Control", "public, max-age=86400") // CDN Caching support

	// Stream directly to client
	if _, err := io.Copy(w, out.Body); err != nil {
		a.Logger.Error("Failed to stream MIDI", "err", err)
	}
}

// GET /songs/{id}/constellation
func (a *App) HandleGetConstellation(w http.ResponseWriter, r *http.Request) {
	songID := chi.URLParam(r, "id")
	if songID == "" {
		a.respondError(w, http.StatusBadRequest, "Missing song ID")
		return
	}

	// Assuming the file was uploaded as MessagePack in the pipeline
	objectKey := fmt.Sprintf("peaks/cm_%s.msgpack", songID)

	out, err := a.S3.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(a.Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		a.Logger.Warn("Constellation map not found in R2", "song_id", songID, "err", err)
		a.respondError(w, http.StatusNotFound, "Constellation map not found")
		return
	}
	defer out.Body.Close()

	// Set headers for binary parsing in browser (JS can fetch as ArrayBuffer)
	w.Header().Set("Content-Type", "application/msgpack")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="cm_%s.msgpack"`, songID))
	w.Header().Set("Cache-Control", "public, max-age=86400")

	// Stream directly to client
	if _, err := io.Copy(w, out.Body); err != nil {
		a.Logger.Error("Failed to stream Constellation Map", "err", err)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func (a *App) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		a.Logger.Error("Failed to encode JSON response", "err", err)
	}
}

func (a *App) respondError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{
		Error: message,
	})
}