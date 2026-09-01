package livedemo

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

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

func NewApp(db *sql.DB, s3Client *s3.Client, bucket string, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}

	var presignClient *s3.PresignClient
	if s3Client != nil {
		presignClient = s3.NewPresignClient(s3Client)
	}

	app := &App{
		DB:               db,
		S3:               s3Client,
		PresignClient:    presignClient,
		Bucket:           bucket,
		Logger:           logger,
		Limiters:         NewIPRateLimiter(rate.Limit(5), 10),
		DisableRateLimit: false,
		LogChan:          make(chan LogEntry, 10000),
	}

	app.startLogWorker()

	return app
}

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

type APIError struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

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

func (a *App) Routes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	r.Use(a.LoggerMiddleware())
	r.Use(a.RateLimitMiddleware())

	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "OK")
	})

	api := r.Group("/api/v1")
	{
		api.POST("/identify", a.HandleIdentify)
		api.GET("/songs/:id/constellation", a.HandleGetConstellation)
	}

	r.NoRoute(gin.WrapH(http.FileServer(http.Dir("./static"))))

	return r
}