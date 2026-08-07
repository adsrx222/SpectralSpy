package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"github.com/adsrx222/SpectralSpy/db"
	"github.com/adsrx222/SpectralSpy/SpectralSpy"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

const MAX_REQUEST_LENGTH = 500000

type IdentifyRequest struct {
	Fingerprints []SpectralSpy.Fingerprint `json:"fingerprints"`
}

type Diagnostics struct {
	MatchScore      int64 `json:"match_score"`
	QueryHashes     int     `json:"query_hashes"`
	UniqueDBHashes  int     `json:"unique_db_hashes"`
	DecodeTimeMs    int64   `json:"decode_time_ms"`
	ExtractTimeMs   int64   `json:"extract_time_ms"`
	DBQueryTimeMs   int64   `json:"db_query_time_ms"`
	StreamTimeMs    int64   `json:"stream_time_ms"`
	MatchTimeMs     int64   `json:"match_time_ms"`
	TotalTimeMs     int64   `json:"total_time_ms"`
}

type IdentifyResponse struct {
	SongID      string      `json:"song_id"`
	Confidence  float64     `json:"confidence"`
	TimeOffset  float64     `json:"time_offset"`
	Diagnostics Diagnostics `json:"diagnostics"`
}

type APIError struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
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
	if val, ok := i.ips.Load(ip); ok {
		return val.(*rate.Limiter)
	}

	limiter := rate.NewLimiter(i.r, i.b)
	actual, _ := i.ips.LoadOrStore(ip, limiter)
	return actual.(*rate.Limiter)
}

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

func (a *App) Routes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	// CORS Configuration
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// apply Middlewares
	r.Use(a.LoggerMiddleware())
	r.Use(a.RateLimitMiddleware())

	// api routes
	api := r.Group("/api/v1")
	{
		api.POST("/identify", a.HandleIdentify)
		api.GET("/songs/:id/midi", a.HandleGetMIDI)
		api.GET("/songs/:id/constellation", a.HandleGetConstellation)
	}

	r.NoRoute(gin.WrapH(http.FileServer(http.Dir("./static"))))

	return r
}

// run initializes dependencies from environment variables and starts the HTTP server
func Run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// initialize Database
	dbUrl := os.Getenv("DB_URL")
	dbToken := os.Getenv("TURSO_AUTH_TOKEN")

	if dbUrl == "" {
		dbUrl = "file:misc/workspace/workspace_hash.sqlite"
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

func (a *App) LoggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next()

		entry := LogEntry{
			Method:     c.Request.Method,
			Path:       c.Request.URL.Path,
			Status:     c.Writer.Status(),
			DurationMs: time.Since(start).Milliseconds(),
			IP:         c.ClientIP(),
		}

		// non-blocking write to logging channel prevents worker stalling
		select {
		case a.LogChan <- entry:
		default:
		}
	}
}

func (a *App) RateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}

// POST /api/v1/identify
// HandleIdentify processes POST /api/v1/identify requests by querying matching audio hashes,
// joining weights from the hash_weight table, and passing entries to SpectralSpy.MatchFingerprints.
func (a *App) HandleIdentify(c *gin.Context) {
	reqStart := time.Now()

	t0 := time.Now()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*1024*1024)

	var req IdentifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		a.respondError(c, http.StatusBadRequest, "Invalid JSON payload")
		return
	}

	reqLen := len(req.Fingerprints)
	if reqLen == 0 {
		a.respondError(c, http.StatusBadRequest, "No fingerprints provided")
		return
	} else if reqLen > MAX_REQUEST_LENGTH {
		a.respondError(c, http.StatusBadRequest, fmt.Sprintf("Maximum %d fingerprints allowed", MAX_REQUEST_LENGTH))
		return
	}
	decodeDuration := time.Since(t0)

	t1 := time.Now()
	uniqueHashes := make(map[uint64]bool, reqLen)
	for _, fp := range req.Fingerprints {
		uniqueHashes[fp.Hash] = true
	}
	extractDuration := time.Since(t1)

	t2 := time.Now()
	placeholders := make([]string, 0, len(uniqueHashes))
	args := make([]interface{}, 0, len(uniqueHashes))

	for hash := range uniqueHashes {
		placeholders = append(placeholders, "?")
		args = append(args, int64(hash))
	}

	// Updated query joining with the 'hash_weight' table per schema.sql
	query := fmt.Sprintf(`
		SELECT ah.hash, ah.song_id, ah.anchor_time, COALESCE(hw.weight, 1.0) AS weight
		FROM audio_hashes ah
		LEFT JOIN hash_weight hw ON CAST(ah.hash AS TEXT) = CAST(hw.hash AS TEXT)
		WHERE ah.hash IN (%s)`, strings.Join(placeholders, ","))
	
	rows, err := a.DB.QueryContext(c.Request.Context(), query, args...)
	if err != nil {
		a.Logger.Error("Database query failed", "err", err)
		a.respondError(c, http.StatusInternalServerError, "Internal database error")
		return
	}
	defer rows.Close()
	dbQueryDuration := time.Since(t2)

	t3 := time.Now()
	dbMap := make(map[uint64][]SpectralSpy.DBEntry)

	for rows.Next() {
		var hash int64
		var songID string
		var anchorTime float64
		var weight float64

		if err := rows.Scan(&hash, &songID, &anchorTime, &weight); err != nil {
			a.Logger.Warn("Failed to scan row", "err", err)
			continue
		}

		uintHash := uint64(hash)
		dbMap[uintHash] = append(dbMap[uintHash], SpectralSpy.DBEntry{
			Hash:       uintHash,
			SongID:     songID,
			AnchorTime: anchorTime,
			Weight:     weight,
		})
	}

	if err := rows.Err(); err != nil {
		a.Logger.Error("Row iteration error", "err", err)
		a.respondError(c, http.StatusInternalServerError, "Database read error")
		return
	}
	streamDuration := time.Since(t3)

	t4 := time.Now()
	if len(dbMap) == 0 {
		a.respondError(c, http.StatusNotFound, "No matching song found")
		return
	}

	// Perform candidate selection and RANSAC offset verification
	bestSongID, bestScore, matchOffset, confidence := SpectralSpy.MatchFingerprints(req.Fingerprints, dbMap)
	if bestSongID == "" {
		a.respondError(c, http.StatusNotFound, "No matching song found")
		return
	}
	matchDuration := time.Since(t4)

	totalDuration := time.Since(reqStart)

	a.respondJSON(c, http.StatusOK, IdentifyResponse{
		SongID:     bestSongID,
		Confidence: confidence,
		TimeOffset: matchOffset,
		Diagnostics: Diagnostics{
			MatchScore:     int64(bestScore),
			QueryHashes:    len(req.Fingerprints),
			UniqueDBHashes: len(dbMap),
			DecodeTimeMs:   decodeDuration.Milliseconds(),
			ExtractTimeMs:  extractDuration.Milliseconds(),
			DBQueryTimeMs:  dbQueryDuration.Milliseconds(),
			StreamTimeMs:   streamDuration.Milliseconds(),
			MatchTimeMs:    matchDuration.Milliseconds(),
			TotalTimeMs:    totalDuration.Milliseconds(),
		},
	})

	a.Logger.Info("HandleIdentify completed",
		"song_id", bestSongID,
		"match_score", bestScore,
		"match_offset_sec", fmt.Sprintf("%.2f", matchOffset),
		"confidence", fmt.Sprintf("%.3f", confidence),
		"query_hashes", len(req.Fingerprints),
		"unique_db_hashes", len(dbMap),
		"timing_decode_ms", decodeDuration.Milliseconds(),
		"timing_extract_ms", extractDuration.Milliseconds(),
		"timing_db_query_ms", dbQueryDuration.Milliseconds(),
		"timing_stream_ms", streamDuration.Milliseconds(),
		"timing_match_ms", matchDuration.Milliseconds(),
		"timing_total_ms", totalDuration.Milliseconds(),
	)
}

// GET /api/v1/songs/:id/midi
func (a *App) HandleGetMIDI(c *gin.Context) {
	if a.PresignClient == nil {
		a.respondError(c, http.StatusServiceUnavailable, "S3 storage is not configured")
		return
	}

	songID := c.Param("id")
	if songID == "" {
		a.respondError(c, http.StatusBadRequest, "Missing song ID")
		return
	}

	objectKey := fmt.Sprintf("midi/%s.mid", songID)

	presignedReq, err := a.PresignClient.PresignGetObject(c.Request.Context(), &s3.GetObjectInput{
		Bucket: aws.String(a.Bucket),
		Key:    aws.String(objectKey),
	}, s3.WithPresignExpires(15*time.Minute))

	if err != nil {
		a.Logger.Error("Failed to generate presigned URL for MIDI", "song_id", songID, "err", err)
		a.respondError(c, http.StatusInternalServerError, "Failed to generate download link")
		return
	}

	c.Redirect(http.StatusFound, presignedReq.URL)
}

// GET /api/v1/songs/:id/constellation
func (a *App) HandleGetConstellation(c *gin.Context) {
	songID := c.Param("id")
	if songID == "" {
		a.respondError(c, http.StatusBadRequest, "Missing song ID")
		return
	}

	objectKey := fmt.Sprintf("peaks/cm_%s.msgpack", songID)

	presignedReq, err := a.PresignClient.PresignGetObject(c.Request.Context(), &s3.GetObjectInput{
		Bucket: aws.String(a.Bucket),
		Key:    aws.String(objectKey),
	}, s3.WithPresignExpires(15*time.Minute))

	if err != nil {
		a.Logger.Error("Failed to generate presigned URL for constellation", "song_id", songID, "err", err)
		a.respondError(c, http.StatusInternalServerError, "Failed to generate download link")
		return
	}

	c.Redirect(http.StatusFound, presignedReq.URL)
}

// Wrapper for successful JSON responses
func (a *App) respondJSON(c *gin.Context, status int, data interface{}) {
	c.JSON(status, data)
}

// Wrapper for Error JSON responses
func (a *App) respondError(c *gin.Context, status int, message string) {
	c.JSON(status, APIError{
		Error: message,
	})
}