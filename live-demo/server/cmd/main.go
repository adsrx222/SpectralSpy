package main

import (
	"bufio"
	"log"
	"os"
	"strings"

	"github.com/adsrx222/SpectralSpy/live-demo/server/src"
)

func main() {
	// Securely require the path to the config file rather than raw secrets
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <path-to-.env-file>", os.Args[0])
	}

	envFilePath := os.Args[1]
	
	envVars, err := readEnvFile(envFilePath)
	if err != nil {
		log.Fatalf("Failed to read environment file: %v", err)
	}

	// Populate the config struct
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

	// Start the application
	if err := livedemo.Run(cfg); err != nil {
		log.Fatalf("Application crashed: %v", err)
	}
}

// readEnvFile securely parses a simple .env file into a map
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
		
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			
			// Strip surrounding quotes if present
			val = strings.Trim(val, `"'`)
			
			envMap[key] = val
		}
	}
	
	return envMap, scanner.Err()
}