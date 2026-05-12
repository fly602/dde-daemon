package autobrightness

import (
	"time"
)

type AutoBrightnessEvent interface {
	GetType() string
	Handle(m *Manager)
}

type StopEvent struct{}

func (e StopEvent) GetType() string {
	return "StopEvent"
}

func (e StopEvent) Handle(m *Manager) {
	logger.Info("[AutoBrightness] Handling StopEvent")
	m.Stop()
}

type IdleEvent struct{}

func (e IdleEvent) GetType() string {
	return "IdleEvent"
}

func (e IdleEvent) Handle(m *Manager) {
	logger.Info("[AutoBrightness] Handling IdleEvent")
}

type ServiceChangeEvent struct {
	ServiceName string
}

func (e ServiceChangeEvent) GetType() string {
	return "ServiceChangeEvent"
}

func (e ServiceChangeEvent) Handle(m *Manager) {
	logger.Infof("[AutoBrightness] Handling ServiceChangeEvent: %s", e.ServiceName)

	if m.sensorClient == nil {
		return
	}

	hasLight, err := m.sensorClient.HasAmbientLight()
	if err != nil {
		logger.Warning("[AutoBrightness] Failed to check ambient light:", err)
		m.supported = false
		return
	}

	if hasLight {
		m.supported = true
		if m.config.Enabled && !m.running {
			m.running = true
			m.startCompensationTimer()
		}
	} else {
		m.supported = false
		if m.running {
			m.stopCompensationTimer()
			m.running = false
		}
	}
}

type LightLevelChangeEvent struct {
	RawLightLevel int
}

func (e LightLevelChangeEvent) GetType() string {
	return "LightLevelChangeEvent"
}

func (e LightLevelChangeEvent) Handle(m *Manager) {
	logger.Infof("[AutoBrightness] Handling LightLevelChangeEvent: %d lux", e.RawLightLevel)

	if !m.running || m.isInManualOverride() {
		return
	}

	m.lastSensorDataTime = time.Now()
	m.processLightChange(e.RawLightLevel)
}

type CompensationLightEvent struct {
	CompensationLightLevel int
}

func (e CompensationLightEvent) GetType() string {
	return "CompensationLightEvent"
}

func (e CompensationLightEvent) Handle(m *Manager) {
	if !m.running || m.isInManualOverride() {
		return
	}

	if m.sensorClient == nil {
		return
	}

	needComp, sensorValue := m.needCompensationWithClient()
	if needComp {
		logger.Infof("[AutoBrightness] Compensation: feeding sensor value %d lux", sensorValue)
		m.processLightChange(sensorValue)
	}
}

type PowerStateChangeEvent struct {
	OnBattery bool
}

func (e PowerStateChangeEvent) GetType() string {
	return "PowerStateChangeEvent"
}

func (e PowerStateChangeEvent) Handle(m *Manager) {
	logger.Infof("[AutoBrightness] Handling PowerStateChangeEvent: onBattery=%v", e.OnBattery)
}

type ConfigurationChangeEvent struct{}

func (e ConfigurationChangeEvent) GetType() string {
	return "ConfigurationChangeEvent"
}

func (e ConfigurationChangeEvent) Handle(m *Manager) {
	logger.Info("[AutoBrightness] Handling ConfigurationChangeEvent")

	config, err := m.getConfig()
	if err != nil {
		logger.Warning("[AutoBrightness] Failed to reload config:", err)
		return
	}

	m.config = config
	m.useTransition = config.UseTransition
	m.applyKalmanFilterConfig()
}

// ManualBrightnessChangeEvent 手动调节亮度事件
type ManualBrightnessChangeEvent struct{}

func (e ManualBrightnessChangeEvent) GetType() string {
	return "ManualBrightnessChangeEvent"
}

func (e ManualBrightnessChangeEvent) Handle(m *Manager) {
	logger.Info("[AutoBrightness] Handling ManualBrightnessChangeEvent")

	if !m.running {
		return
	}

	if m.config.ManualAdjustDisablesAutoMode {
		logger.Info("[AutoBrightness] Manual brightness change detected, disabling auto brightness mode")
		m.config.Enabled = false
		m.stopCompensationTimer()
		m.running = false
		m.enabled = false
		if m.onAutoBrightnessDisabled != nil {
			m.onAutoBrightnessDisabled()
		}
		return
	}

	// 默认行为：临时暂停
	m.resetHistoryState()
	m.manualOverride = time.Now()
	logger.Infof("[AutoBrightness] Manual brightness change detected, pausing auto adjustment for %d seconds", m.config.ManualOverrideDuration)

	if m.sensorClient != nil && m.sensorClient.IsClaimed() {
		err := m.sensorClient.ReleaseLight()
		if err != nil {
			logger.Warning("[AutoBrightness] Failed to release light sensor on manual override:", err)
		}
	}
}
