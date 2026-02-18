package server

import (
	commonconfig "github.com/openjobspec/ojs-go-backend-common/config"
)

// Config holds server configuration from environment variables.
type Config struct {
	commonconfig.BaseConfig
	NatsURL string
}

// LoadConfig reads configuration from environment variables with defaults.
func LoadConfig() Config {
	return Config{
		BaseConfig: commonconfig.LoadBaseConfig(),
		NatsURL:    commonconfig.GetEnv("NATS_URL", "nats://localhost:4222"),
	}
}
