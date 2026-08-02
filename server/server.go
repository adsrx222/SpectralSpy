package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
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

	"spectralspy/db"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

const MAX_REQUEST_LENGTH = 500

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

type OffsetKey struct {
	SongID     string
	AnchorTime float64
}

type LogEntry struct {
	Method     string
	Path       string
	Status     int
	DurationMs int64
	IP         string
}

type App struct {
	DB               *sql.DB
	S3               *s3.Client
	PresignClient    *s3.PresignClient
	Bucket           string
	Logger           *slog.Logger
	Limiters         *IPRateLimiter
	DisableRateLimit bool
	LogChan          chan LogEntry
}

type IPRateLimiter struct {
	ips sync.Map
	r   rate.Limit
	b   int
}

func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter {
	return &IPRateLimiter{
		r: r,
		b: b,
	}
}

func (i *IPRateLimiter) GetLimiter(ip string) *rate.Limiter {
	// Fast path: Atomic read with zero mutex overhead
	if val, ok := i.ips.Load(ip); ok {
		return val.(*rate.Limiter)
	}

	// Slow path: Create new limiter and store safely without race conditions
	limiter := rate.NewLimiter(i.r, i.b)
	actual, _ := i.ips.LoadOrStore(ip, limiter)
	return actual.(*rate.Limiter)
}

// NewApp initializes and returns an app instance with configured dependencies
// NewApp initializes and returns an app instance with configured dependencies
func NewApp(dbConn *sql.DB, s3Client *s3.Client, bucket string, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}

	var presignClient *s3.PresignClient
	if s3Client != nil {
		presignClient = s3.NewPresignClient(s3Client)
	}

	app := &App{
		DB:               dbConn,
		S3:               s3Client,
		PresignClient:    presignClient,
		Bucket:           bucket,
		Logger:           logger,
		Limiters:         NewIPRateLimiter(rate.Limit(5), 10), // 5 req/sec, burst 10
		DisableRateLimit: false,
		LogChan:          make(chan LogEntry, 10000), // Non-blocking async log buffer
	}

	app.startLogWorker()

	return app
}

// Background goroutine to handle I/O logging off the critical HTTP path
func (a *App) startLogWorker() {
	go func() {
		for entry := range a.LogChan {
			a.Logger.Info("HTTP Request",
				"method", entry.Method,
				"path", entry.Path,
				"status", entry.Status,
				"duration_ms", entry.DurationMs,
				"ip", entry.IP,
			)
		}
	}()
}

// routes builds and returns the HTTP router for the application
func (a *App) Routes() http.Handler {
	r := chi.NewRouter()

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/identify", a.HandleIdentify)
		r.Get("/songs/{id}/midi", a.HandleGetMIDI)
		r.Get("/songs/{id}/constellation", a.HandleGetConstellation)
	})

	return r
}

// runs & initializes dependencies from environment variables and starts the HTTP server
func Run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// initialize Database
	dbUrl := os.Getenv("DB_URL")
	dbToken := os.Getenv("TURSO_AUTH_TOKEN")

	if dbUrl == "" {
		dbUrl = "file:hashes.sqlite"
	}

	if dbToken != "" && !strings.Contains(dbUrl, "authToken=") {
		if strings.Contains(dbUrl, "?") {
			dbUrl = fmt.Sprintf("%s&authToken=%s", dbUrl, dbToken)
		} else {
			dbUrl = fmt.Sprintf("%s?authToken=%s", dbUrl, dbToken)
		}
	}

	dbConn, err := sql.Open("libsql", dbUrl)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer dbConn.Close()

	// enable db pragmas on startup
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA cache_size=-64000;",
		"PRAGMA temp_store=MEMORY;",
		"PRAGMA busy_timeout=5000;",
	}
	for _, pragma := range pragmas {
		if _, err := dbConn.Exec(pragma); err != nil {
			logger.Warn("Failed to execute DB startup PRAGMA", "pragma", pragma, "err", err)
		}
	}

	if err := db.InitSchema(dbConn); err != nil {
		return fmt.Errorf("failed to initialize schema: %w", err)
	}

	// initialize s3 Client
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			os.Getenv("R2_ACCESS_KEY_ID"),
			os.Getenv("R2_SECRET_ACCESS_KEY"),
			"",
		)),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Endpoint := os.Getenv("S3_ENDPOINT")
	if s3Endpoint == "" {
		s3Endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", os.Getenv("R2_ACCOUNT_ID"))
	}

	r2Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3Endpoint)
		o.UsePathStyle = true
	})

	// init app & server
	app := NewApp(dbConn, r2Client, os.Getenv("R2_BUCKET_NAME"), logger)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logger.Info("Starting server", "port", port)
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: app.Routes(),
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

func (a *App) LoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		entry := LogEntry{
			Method:     r.Method,
			Path:       r.URL.Path,
			Status:     ww.Status(),
			DurationMs: time.Since(start).Milliseconds(),
			IP:         r.RemoteAddr,
		}

		// non-blocking write to logging channel prevents worker stalling
		select {
			case a.LogChan <- entry:
			default:
		}
	})
}

func (a *App) RateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// POST /identify
func (a *App) HandleIdentify(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()

	// decode JSON & request validation
	t0 := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024)

	var req IdentifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.respondError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}

	reqLen := len(req.Fingerprints)

	if reqLen == 0 {
		a.respondError(w, http.StatusBadRequest, "No fingerprints provided")
		return
	} else if reqLen > MAX_REQUEST_LENGTH {
		a.respondError(w, http.StatusBadRequest, fmt.Sprintf("Maximum %d fingerprints allowed per request", MAX_REQUEST_LENGTH))
		return
	}
	decodeDuration := time.Since(t0)

	// group timestamps by hash
	t1 := time.Now()
	queryMap := make(map[uint64][]float64, reqLen)
	for _, fp := range req.Fingerprints {
		queryMap[fp.Hash] = append(queryMap[fp.Hash], fp.AnchorTime)
	}
	part1Duration := time.Since(t1)

	// build SQL query parameters & execute
	t2 := time.Now()
	uniqueCount := len(queryMap)
	placeholders := make([]string, 0, uniqueCount)
	args := make([]interface{}, 0, uniqueCount)

	for hash := range queryMap {
		placeholders = append(placeholders, "?")
		args = append(args, int64(hash))
	}

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
	part2Duration := time.Since(t2)

	// fast streaming row iteration
	t3 := time.Now()
	const HOP_SIZE = 2048.0
	globalHist := make(map[OffsetKey]int, uniqueCount)

	for rows.Next() {
		var hash int64
		var songID string
		var dbAnchorTime float64

		if err := rows.Scan(&hash, &songID, &dbAnchorTime); err != nil {
			continue
		}

		uHash := uint64(hash)
		if queryAnchors, exists := queryMap[uHash]; exists {
			// len(queryAnchors) is almost always = 1	
			for _, qAnchorTime := range queryAnchors {
				quantised := math.Round((dbAnchorTime - qAnchorTime) / HOP_SIZE)
				key := OffsetKey{SongID: songID, AnchorTime: quantised}
				globalHist[key]++
			}
		}
	}
	part3Duration := time.Since(t3)

	// check for errors during iteration & find top matches 
	t4 := time.Now()
	if err := rows.Err(); err != nil {
		a.Logger.Error("Row iteration error", "err", err)
		a.respondError(w, http.StatusInternalServerError, "Database read error")
		return
	}

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
	part4Duration := time.Since(t4)

	// calculate total E2E & section percentages
	totalDuration := time.Since(reqStart)

	calcPct := func(d time.Duration) float64 {
		if totalDuration == 0 {
			return 0
		}
		return (float64(d.Nanoseconds()) / float64(totalDuration.Nanoseconds())) * 100.0
	}

	a.Logger.Info("[HandleIdentify Timing Breakdown]",
		"decode_val_time", decodeDuration.String(),
		"decode_val_pct", fmt.Sprintf("%.2f%%", calcPct(decodeDuration)),
		"part1_group_time", part1Duration.String(),
		"part1_pct", fmt.Sprintf("%.2f%%", calcPct(part1Duration)),
		"part2_db_query_time", part2Duration.String(),
		"part2_pct", fmt.Sprintf("%.2f%%", calcPct(part2Duration)),
		"part3_stream_iter_time", part3Duration.String(),
		"part3_pct", fmt.Sprintf("%.2f%%", calcPct(part3Duration)),
		"part4_scoring_time", part4Duration.String(),
		"part4_pct", fmt.Sprintf("%.2f%%", calcPct(part4Duration)),
		"total_e2e_time", totalDuration.String(),
	)

	if bestSong == "" {
		a.respondError(w, http.StatusNotFound, "No matching song found")
		return
	}

	var conf float64
	if secondBestVotes == 0 {
		conf = float64(firstBestVotes)
	} else {
		conf = float64(firstBestVotes) / float64(secondBestVotes)
	}

	a.respondJSON(w, http.StatusOK, IdentifyResponse{
		SongID:     bestSong,
		Confidence: conf,
		TimeOffset: bestOffset,
	})
}

// GET /songs/{id}/midi
func (a *App) HandleGetMIDI(w http.ResponseWriter, r *http.Request) {
	if a.PresignClient == nil {
		a.respondError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
		return
	}
	
	songID := chi.URLParam(r, "id")
	if songID == "" {
		a.respondError(w, http.StatusBadRequest, "Missing song ID")
		return
	}

	objectKey := fmt.Sprintf("midi/%s.mid", songID)

	presignedReq, err := a.PresignClient.PresignGetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(a.Bucket),
		Key:    aws.String(objectKey),
	}, s3.WithPresignExpires(15*time.Minute))

	if err != nil {
		a.Logger.Error("Failed to generate presigned URL for MIDI", "song_id", songID, "err", err)
		a.respondError(w, http.StatusInternalServerError, "Failed to generate download link")
		return
	}

	http.Redirect(w, r, presignedReq.URL, http.StatusFound)
}

// GET /songs/{id}/constellation
func (a *App) HandleGetConstellation(w http.ResponseWriter, r *http.Request) {
	songID := chi.URLParam(r, "id")
	if songID == "" {
		a.respondError(w, http.StatusBadRequest, "Missing song ID")
		return
	}

	objectKey := fmt.Sprintf("peaks/cm_%s.msgpack", songID)

	presignedReq, err := a.PresignClient.PresignGetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(a.Bucket),
		Key:    aws.String(objectKey),
	}, s3.WithPresignExpires(15*time.Minute))

	if err != nil {
		a.Logger.Error("Failed to generate presigned URL for constellation", "song_id", songID, "err", err)
		a.respondError(w, http.StatusInternalServerError, "Failed to generate download link")
		return
	}

	http.Redirect(w, r, presignedReq.URL, http.StatusFound)
}

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