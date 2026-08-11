package livedemo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	SpectralSpy "github.com/adsrx222/SpectralSpy/src"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// Config holds the environment variables required for the application.
type Config struct {
	DBUrl             string
	DBAuthToken       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	S3Endpoint        string
	R2AccountID       string
	R2BucketName      string
	Port              string
}

// Run initializes dependencies from the provided Config and starts the
// HTTP server for this reference live-demo application.
func Run(cfg Config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// initialize Database
	dbUrl := cfg.DBUrl
	dbToken := cfg.DBAuthToken

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

	if err := SpectralSpy.InitSchema(dbConn); err != nil {
		return fmt.Errorf("failed to initialize schema: %w", err)
	}

	// initialize s3 Client
	awsCfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.R2AccessKeyID,
			cfg.R2SecretAccessKey,
			"",
		)),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Endpoint := cfg.S3Endpoint
	if s3Endpoint == "" {
		s3Endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.R2AccountID)
	}

	r2Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3Endpoint)
		o.UsePathStyle = true
	})

	// init app & server
	app := NewApp(dbConn, r2Client, cfg.R2BucketName, logger)

	port := cfg.Port
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