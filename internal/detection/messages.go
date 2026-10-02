package detection

import (
	"errors"
	"fmt"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

var ErrNoRaiser = errors.New("anomaly detector requires an alarm raiser")

func messageOverpressure(run []model.Sample, limit float64) string {
	peak := run[0].Value
	for _, sample := range run[1:] {
		if sample.Value > peak {
			peak = sample.Value
		}
	}
	duration := run[len(run)-1].ObservedAt.Sub(run[0].ObservedAt).Truncate(time.Second)
	return fmt.Sprintf("sustained pressure %.1f kPa exceeds %.0f kPa limit for %s (peak %.1f kPa, %d samples)",
		run[len(run)-1].Value, limit, duration, peak, len(run))
}

func messageLeak(levelDrop float64, pressure []model.Sample, pressureFloor float64) string {
	peakPressure := pressure[0].Value
	for _, sample := range pressure[1:] {
		if sample.Value > peakPressure {
			peakPressure = sample.Value
		}
	}
	return fmt.Sprintf("suspected nitrogen leak: level fell %.1f mm while pressure stayed elevated at >= %.0f kPa (peak %.1f kPa)",
		levelDrop, pressureFloor, peakPressure)
}
