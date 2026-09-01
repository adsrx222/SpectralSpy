package main

import (
	"bufio"
	"log"
	"os"
	"strings"

	"github.com/adsrx222/SpectralSpy/live-demo/server/src"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <path-to-.env-file>", os.Args[0])
	}

	envVars, err := readEnvFile(os.Args[1])
	if err != nil {
		log.Fatalf("Failed to read environment file: %v", err)
	}

	cfg := livedemo.Config{
		DBUrl:             envVars["DB_URL"],
		DBAuthToken:       envVars["DB_AUTH_TOKEN"],
		R2AccessKeyID:     envVars["R2_ACCESS_KEY_ID"],
		R2SecretAccessKey: envVars["R2_SECRET_ACCESS_KEY"],
		S3Endpoint:        envVars["S3_ENDPOINT"],
		R2AccountID:       envVars["R2_ACCOUNT_ID"],
		R2BucketName:      envVars["R2_BUCKET_NAME"],
		Port:              envVars["PORT"],
	}

	if err := livedemo.Run(cfg); err != nil {
		log.Fatalf("Application crashed: %v", err)
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