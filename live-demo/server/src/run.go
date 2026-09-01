package livedemo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/adsrx222/SpectralSpy/src"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

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

func Run(cfg Config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	dsn := fmt.Sprintf("%s?token=%s", cfg.DBUrl, cfg.DBAuthToken)

	dbConn, err := sql.Open("d1-proxy", dsn)
	if err != nil {
		return fmt.Errorf("failed to open database connection: %w", err)
	}
	defer dbConn.Close()

	if err := SpectralSpy.InitSchema(dbConn); err != nil {
		return fmt.Errorf("failed to initialize schema: %w", err)
	}

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