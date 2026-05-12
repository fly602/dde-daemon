package autobrightness

import (
	"math"
	"time"

	"github.com/linuxdeepin/dde-daemon/display1/brightness"
)

func (m *Manager) isInManualOverride() bool {
	if m.manualOverride.IsZero() {
		return false
	}
	duration := time.Duration(m.config.ManualOverrideDuration) * time.Second
	return time.Since(m.manualOverride) < duration
}

func (m *Manager) resetHistoryState() {
	m.lastLightLevel = -1
	m.lastBrightness = -1
	m.lastAdjustTime = time.Time{}
	m.lastSensorDataTime = time.Time{}
	if m.kalmanFilter != nil {
		m.kalmanFilter.Reset()
	}
}

func (m *Manager) processLightChange(rawLightLevel int) {
	if m.kalmanFilter == nil {
		logger.Warning("[AutoBrightness] Kalman filter is nil, skipping")
		return
	}

	estimate := m.kalmanFilter.Update(float64(rawLightLevel))
	filteredLightLevel := int(estimate)
	logger.Infof("[AutoBrightness] Kalman filter: raw=%d -> filtered=%d", rawLightLevel, filteredLightLevel)

	targetBrightness := m.calculateTargetBrightness(filteredLightLevel)

	if !m.shouldAdjustBrightness(filteredLightLevel, targetBrightness) {
		return
	}

	err := m.setBrightness(targetBrightness)
	if err != nil {
		logger.Warningf("[AutoBrightness] Failed to set brightness: %v", err)
		return
	}

	m.lastLightLevel = filteredLightLevel
	m.lastBrightness = targetBrightness
	m.lastAdjustTime = time.Now()

	logger.Infof("[AutoBrightness] Brightness adjusted: %d -> %.1f%%", filteredLightLevel, targetBrightness*100)
}

func (m *Manager) calculateTargetBrightness(lightLevel int) float64 {
	if brightness.HasAutoBrightnessCurve() {
		br := brightness.GetAutoBrightnessValue(lightLevel)
		if br >= 0 {
			if br < 0.0 {
				br = 0.0
			} else if br > 1.0 {
				br = 1.0
			}
			minBrightness := 0.1
			if br < minBrightness {
				br = minBrightness
			}
			return br
		}
	}

	adjustedLevel := float64(lightLevel) * m.config.Sensitivity
	br := adjustedLevel / 1024.0

	if br < 0.0 {
		br = 0.0
	} else if br > 1.0 {
		br = 1.0
	}

	minBrightness := 0.1
	if br < minBrightness {
		br = minBrightness
	}
	return br
}

func (m *Manager) shouldAdjustBrightness(lightLevel int, targetBrightness float64) bool {
	now := time.Now()

	if m.lastLightLevel >= 0 {
		lightChange := math.Abs(float64(lightLevel - m.lastLightLevel))
		if lightChange < m.config.ChangeThreshold {
			return false
		}
	}

	if !m.lastAdjustTime.IsZero() {
		timeSinceLastAdjust := now.Sub(m.lastAdjustTime)
		minInterval := time.Duration(m.config.PollingInterval) * time.Second
		if timeSinceLastAdjust < minInterval {
			return false
		}
	}

	if m.lastBrightness >= 0 {
		brightnessChange := math.Abs(targetBrightness - m.lastBrightness)
		if brightnessChange < m.config.BrightnessChangeThreshold {
			return false
		}
	}

	return true
}

func (m *Manager) needCompensationWithClient() (bool, int) {
	if m.sensorClient == nil {
		return false, 0
	}

	if m.lastSensorDataTime.IsZero() {
		return false, 0
	}

	if time.Since(m.lastSensorDataTime) < sensorDataTimeout {
		return false, 0
	}

	if m.kalmanFilter == nil {
		return false, 0
	}

	sensorValue, err := m.sensorClient.GetLightLevel()
	if err != nil {
		return false, 0
	}

	filterOutput := m.kalmanFilter.GetEstimate()
	diff := math.Abs(filterOutput - float64(sensorValue))

	return diff > compensationThreshold, sensorValue
}

func (m *Manager) setBrightness(value float64) error {
	if m.setBrightnessFunc == nil {
		return nil
	}
	return m.setBrightnessFunc(value)
}
