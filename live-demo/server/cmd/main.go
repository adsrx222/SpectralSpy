package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/adsrx222/SpectralSpy/live-demo/server/src"
	"github.com/adsrx222/SpectralSpy/src"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <path-to-.env-file>", os.Args[0])
	}

	envVars, err := readEnvFile(os.Args[1])
	if err != nil {
		log.Fatalf("Failed to read environment file: %v", err)
	}

	// initialize cloudflare d1
	dsn := fmt.Sprintf("%s?token=%s", envVars["DB_URL"], envVars["DB_AUTH_TOKEN"])
	dbConn, err := sql.Open("d1-proxy", dsn)
	if err != nil {
		log.Fatalf("Failed to open D1 connection: %v", err)
	}
	defer dbConn.Close()

	if err := SpectralSpy.InitSchema(dbConn); err != nil {
		log.Fatalf("Failed to initialize schema: %v", err)
	}

	// initialize cloudflare R2
	awsCfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			envVars["R2_ACCESS_KEY_ID"],
			envVars["R2_SECRET_ACCESS_KEY"],
			"",
		)),
	)
	if err != nil {
		log.Fatalf("Failed to load AWS/R2 config: %v", err)
	}

	s3Endpoint := envVars["S3_ENDPOINT"]
	if s3Endpoint == "" {
		s3Endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", envVars["R2_ACCOUNT_ID"])
	}

	r2Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3Endpoint)
		o.UsePathStyle = true
	})

	// setup and start server
	app := livedemo.NewApp(dbConn, r2Client, envVars["R2_BUCKET_NAME"], logger)

	port := envVars["PORT"]
	if port == "" {
		port = "8080"
	}

	logger.Info("Starting server", "port", port)
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: app.Routes(),
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server error: %v", err)
	}
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	envMap := make(map[string]string)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			envMap[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		}
	}
	return envMap, scanner.Err()
}