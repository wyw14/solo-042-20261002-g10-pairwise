package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wyw14/cry-98/internal/detect"
)

type config struct {
	Address           string
	DataDir           string
	ShutdownTimeout   time.Duration
	ProbeStaleAfter   time.Duration
	NotificationRetry time.Duration
	Detect            detect.Config
}

func envDuration(name string, fallback time.Duration) time.Duration {
	if raw := os.Getenv(name); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			return parsed
		}
	}
	return fallback
}

func envFloat(name string, fallback float64) float64 {
	if raw := os.Getenv(name); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if raw := os.Getenv(name); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			return parsed
		}
	}
	return fallback
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
	detectCfg := detect.DefaultConfig()
	detectCfg.DetectWindow = envDuration("CRYOSAFE_DETECT_WINDOW", detectCfg.DetectWindow)
	detectCfg.MinSamples = envInt("CRYOSAFE_DETECT_MIN_SAMPLES", detectCfg.MinSamples)
	detectCfg.StaleAfter = envDuration("CRYOSAFE_PROBE_STALE_AFTER", 2*time.Minute)
	detectCfg.OverPressureKPa = envFloat("CRYOSAFE_OVERPRESSURE_KPA", detectCfg.OverPressureKPa)
	detectCfg.LeakPressureKPa = envFloat("CRYOSAFE_LEAK_PRESSURE_KPA", detectCfg.LeakPressureKPa)
	detectCfg.LeakPressureDropKPa = envFloat("CRYOSAFE_LEAK_PRESSURE_DROP_KPA", detectCfg.LeakPressureDropKPa)
	detectCfg.LeakLevelMM = envFloat("CRYOSAFE_LEAK_LEVEL_MM", detectCfg.LeakLevelMM)
	detectCfg.LeakLevelDropMM = envFloat("CRYOSAFE_LEAK_LEVEL_DROP_MM", detectCfg.LeakLevelDropMM)
	detectCfg.LeakTemperatureC = envFloat("CRYOSAFE_LEAK_TEMP_C", detectCfg.LeakTemperatureC)
	detectCfg.LeakTemperatureDropC = envFloat("CRYOSAFE_LEAK_TEMP_DROP_C", detectCfg.LeakTemperatureDropC)
	if err := detectCfg.Validate(); err != nil {
		return config{}, err
	}
	return config{
		Address:           address,
		DataDir:           dataDir,
		ShutdownTimeout:   10 * time.Second,
		ProbeStaleAfter:   detectCfg.StaleAfter,
		NotificationRetry: 5 * time.Second,
		Detect:            detectCfg,
	}, nil
}
