// Package livedemo composes SpectralSpy's core identify handler (from the
// server package, via HandleIdentify in identify.go) together with this
// project's demo-specific concerns: rate limiting, structured async request
// logging, CORS, static file serving, and the MIDI/constellation download
// endpoints backed by S3.
//
// Nothing in this package is required to use server.Identify() elsewhere —
// this is one particular application built on top of it.
package livedemo

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"database/sql"
	"log/slog"
)

// ─────────────────────────────────────────────────────────────────────────
// App
// ─────────────────────────────────────────────────────────────────────────

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

// NewApp initializes and returns an app instance with configured dependencies.
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
		LogChan:          make(chan LogEntry, 10000), // non-blocking async log buffer
	}

	app.startLogWorker()

	return app
}

// background goroutine to handle I/O logging off the critical HTTP path
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

// ─────────────────────────────────────────────────────────────────────────
// Rate limiting & middleware
// ─────────────────────────────────────────────────────────────────────────

type LogEntry struct {
	Method     string
	Path       string
	Status     int
	DurationMs int64
	IP         string
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
		if !a.DisableRateLimit {
			limiter := a.Limiters.GetLimiter(c.ClientIP())
			if !limiter.Allow() {
				respondError(c, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.")
				c.Abort()
				return
			}
		}
		
		c.Next()
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Media endpoints (MIDI / constellation, presigned S3 downloads)
// ─────────────────────────────────────────────────────────────────────────

type APIError struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// GET /api/v1/songs/:id/constellation
func (a *App) HandleGetConstellation(c *gin.Context) {
	if a.PresignClient == nil {
		respondError(c, http.StatusServiceUnavailable, "S3 storage is not configured")
		return
	}

	songID := c.Param("id")
	if songID == "" {
		respondError(c, http.StatusBadRequest, "Missing song ID")
		return
	}

	objectKey := fmt.Sprintf("peaks/cm_%s.msgpack", songID)

	presignedReq, err := a.PresignClient.PresignGetObject(c.Request.Context(), &s3.GetObjectInput{
		Bucket: aws.String(a.Bucket),
		Key:    aws.String(objectKey),
	}, s3.WithPresignExpires(15*time.Minute))

	if err != nil {
		a.Logger.Error("Failed to generate presigned URL for constellation", "song_id", songID, "err", err)
		respondError(c, http.StatusInternalServerError, "Failed to generate download link")
		return
	}

	c.Redirect(http.StatusFound, presignedReq.URL)
}

func respondJSON(c *gin.Context, status int, data interface{}) {
	c.JSON(status, data)
}

func respondError(c *gin.Context, status int, message string) {
	c.JSON(status, APIError{
		Error: message,
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Routes
// ─────────────────────────────────────────────────────────────────────────

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
		// livedemo's own HandleIdentify (in identify.go) wraps
		// server.Identify() (the core matching logic) and layers on
		// ?include=artist,song support via livedemo's own songs table.
		api.POST("/identify", a.HandleIdentify)
		api.GET("/songs/:id/constellation", a.HandleGetConstellation)
	}

	r.NoRoute(gin.WrapH(http.FileServer(http.Dir("./static"))))

	return r
}