// Package envflag provides small helpers for reading environment variables
// as flag defaults, so every setting can be configured either via CLI flag
// or via environment variable (e.g. from a Docker Compose env_file) — flags
// always take precedence if explicitly passed.
package envflag

import (
	"os"
	"strconv"
	"time"
)

// String returns the value of the given environment variable, or fallback
// if it's unset or empty.
func String(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Bool returns the parsed boolean value of the given environment variable,
// or fallback if it's unset, empty, or not a valid boolean.
func Bool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// Int returns the parsed integer value of the given environment variable,
// or fallback if it's unset, empty, or not a valid integer.
func Int(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// Duration returns the parsed time.Duration value of the given environment
// variable (e.g. "5s", "2m"), or fallback if it's unset, empty, or not a
// valid duration.
func Duration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return parsed
}
