package server

import (
	"os"
	"strconv"
)

// Config holds server configuration from environment variables.
type Config struct {
	Port     string
	GRPCPort string
	NatsURL  string
}

// LoadConfig reads configuration from environment variables with defaults.
func LoadConfig() Config {
	return Config{
		Port:     getEnv("OJS_PORT", "8080"),
		GRPCPort: getEnv("OJS_GRPC_PORT", "9090"),
		NatsURL:  getEnv("NATS_URL", "nats://localhost:4222"),
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return defaultVal
}
