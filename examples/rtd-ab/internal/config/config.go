// SPDX-License-Identifier: Apache-2.0

// Package config reads the runtime environment variables Index Cloud sets at
// container start, per "Building your container → Environment variables", plus
// the local-run knobs of the rtd-ab experiment.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the resolved runtime configuration.
type Config struct {
	// GRPCPort — port the gRPC RTBExtensionPoint service (and gRPC health) listen
	// on. Set by Index via GRPC_PORT; documented default 50051.
	GRPCPort int
	// ServerPort — port for the HTTP /metrics, /heartbeat, /healthcheck endpoints.
	// Set by Index via SERVER_PORT (no documented default; 8080 for local runs).
	ServerPort int
	// ModelPath — directory model data files are mounted into (MODEL_PATH). This
	// container loads no model; it only reports what is mounted there.
	ModelPath string
	// APIVersion — this container's version, reported in response metadata.
	APIVersion string
	// SummaryEvery — interval of the per-arm summary log line
	// (RTD_AB_SUMMARY_SEC, default 60); 0 disables it. At most one line per arm
	// per interval, and none for an arm with no requests in the interval.
	SummaryEvery time.Duration
}

// Load resolves configuration from the environment with local-dev defaults so
// the container also runs standalone.
func Load() Config {
	return Config{
		GRPCPort:     getInt("GRPC_PORT", 50051),
		ServerPort:   getInt("SERVER_PORT", 8080),
		ModelPath:    getStr("MODEL_PATH", "/models"),
		APIVersion:   getStr("APP_VERSION", "0.1.0"),
		SummaryEvery: time.Duration(getInt("RTD_AB_SUMMARY_SEC", 60)) * time.Second,
	}
}

func getStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}
