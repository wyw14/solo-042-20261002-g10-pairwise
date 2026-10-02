package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type config struct {
	Address           string
	DataDir           string
	ShutdownTimeout   time.Duration
	ProbeStaleAfter   time.Duration
	NotificationRetry time.Duration
	Detection         detectionConfig
}

type detectionConfig struct {
	Window           time.Duration
	MinDuration      time.Duration
	MinSamples       int
	MaxSampleAge     time.Duration
	PressureLimitKPa float64
	LeakLevelDropMM  float64
	LeakPressureKPa  float64
}

func defaultDetectionConfig() detectionConfig {
	return detectionConfig{
		Window:           30 * time.Second,
		MinDuration:      10 * time.Second,
		MinSamples:       3,
		MaxSampleAge:     2 * time.Minute,
		PressureLimitKPa: 600,
		LeakLevelDropMM:  20,
		LeakPressureKPa:  300,
	}
}

func loadConfig() (config, error) {
	dataDir := os.Getenv("CRYOSAFE_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "cryosafe-data")
	}
	address := os.Getenv("CRYOSAFE_ADDR")
	if address == "" {
		address = "127.0.0.1:19698"
	}
	if address == "" || dataDir == "" {
		return config{}, errors.New("service address and data directory are required")
	}
	cfg := config{
		Address:           address,
		DataDir:           dataDir,
		ShutdownTimeout:   10 * time.Second,
		ProbeStaleAfter:   2 * time.Minute,
		NotificationRetry: 5 * time.Second,
		Detection:         defaultDetectionConfig(),
	}
	if value := os.Getenv("CRYOSAFE_DETECTION_WINDOW"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return config{}, errors.New("invalid CRYOSAFE_DETECTION_WINDOW")
		}
		cfg.Detection.Window = parsed
	}
	if value := os.Getenv("CRYOSAFE_DETECTION_MIN_DURATION"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return config{}, errors.New("invalid CRYOSAFE_DETECTION_MIN_DURATION")
		}
		cfg.Detection.MinDuration = parsed
	}
	if value := os.Getenv("CRYOSAFE_DETECTION_MIN_SAMPLES"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return config{}, errors.New("invalid CRYOSAFE_DETECTION_MIN_SAMPLES")
		}
		cfg.Detection.MinSamples = parsed
	}
	if value := os.Getenv("CRYOSAFE_PROBE_MAX_AGE"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return config{}, errors.New("invalid CRYOSAFE_PROBE_MAX_AGE")
		}
		cfg.Detection.MaxSampleAge = parsed
	}
	if value := os.Getenv("CRYOSAFE_PRESSURE_LIMIT_KPA"); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return config{}, errors.New("invalid CRYOSAFE_PRESSURE_LIMIT_KPA")
		}
		cfg.Detection.PressureLimitKPa = parsed
	}
	if value := os.Getenv("CRYOSAFE_LEAK_LEVEL_DROP_MM"); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return config{}, errors.New("invalid CRYOSAFE_LEAK_LEVEL_DROP_MM")
		}
		cfg.Detection.LeakLevelDropMM = parsed
	}
	if value := os.Getenv("CRYOSAFE_LEAK_PRESSURE_KPA"); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return config{}, errors.New("invalid CRYOSAFE_LEAK_PRESSURE_KPA")
		}
		cfg.Detection.LeakPressureKPa = parsed
	}
	return cfg, nil
}
