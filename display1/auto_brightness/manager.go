package autobrightness

import (
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/linuxdeepin/dde-daemon/display1/brightness"
	configManager "github.com/linuxdeepin/go-dbus-factory/org.desktopspec.ConfigManager"
	sensorproxy "github.com/linuxdeepin/go-dbus-factory/system/net.hadess.sensorproxy"
	ofdbus "github.com/linuxdeepin/go-dbus-factory/system/org.freedesktop.dbus"
	"github.com/linuxdeepin/go-lib/dbusutil"
	"github.com/linuxdeepin/go-lib/log"
)

var logger = log.NewLogger("daemon/display")

const (
	compensationInterval  = 150 * time.Millisecond
	sensorDataTimeout     = 1 * time.Second
	compensationThreshold = 5.0
)

const (
	DSettingsAutoBrightnessAppID = "org.deepin.dde.daemon"
	DSettingsAutoBrightnessName  = "org.deepin.Display.AutoBrightness"

	// 渐变亮度配置
	DSettingsKeyABTransitionEnabled      = "transition-enabled"
	DSettingsKeyABTransitionDuration     = "transition-duration"
	DSettingsKeyABTransitionStepInterval = "transition-step-interval"

	// 自动亮度配置（独立配置文件）
	DSettingsKeyABEnabled                      = "enabled"
	DSettingsKeyABSensitivity                  = "sensitivity"
	DSettingsKeyABChangeThreshold              = "change-threshold"
	DSettingsKeyABPollingInterval              = "polling-interval"
	DSettingsKeyABManualOverride               = "manual-override-duration"
	DSettingsKeyABManualAdjustDisablesAutoMode = "manual-adjust-disables-auto-mode"
	DSettingsKeyABUseTransition                = "use-transition"
	DSettingsKeyABBrightnessChangeThreshold    = "brightness-change-threshold"
	DSettingsKeyABCurve                        = "lux-brightness-curve"

	// 卡尔曼滤波器配置
	DSettingsKeyABKalmanProcessNoise     = "kalman-process-noise"
	DSettingsKeyABKalmanMeasurementNoise = "kalman-measurement-noise"
	DSettingsKeyABKalmanWindowSize       = "kalman-window-size"
)

// AutoBrightnessConfig 自动亮度配置结构体
type AutoBrightnessConfig struct {
	Enabled                      bool    `json:"enabled"`                          // 是否启用自动亮度
	Sensitivity                  float64 `json:"sensitivity"`                      // 敏感度 (0.1-3.0)
	PollingInterval              int     `json:"polling_interval"`                 // 轮询间隔(秒) (1-60)
	ChangeThreshold              float64 `json:"change_threshold"`                 // 变化阈值 (1.0-50.0)
	BrightnessChangeThreshold    float64 `json:"brightness_change_threshold"`      // 亮度变化阈值 (0.01-1.0)
	ManualOverrideDuration       int     `json:"manual_override_duration"`         // 手动调节暂停时间(秒) (60-1800)
	ManualAdjustDisablesAutoMode bool    `json:"manual_adjust_disables_auto_mode"` // 手动调节是否禁用自动模式
	UseTransition                bool    `json:"use_transition"`                   // 自动调节时是否使用渐变效果
	KalmanProcessNoise           float64 `json:"kalman_process_noise"`             // 卡尔曼滤波器过程噪声协方差 Q
	KalmanMeasurementNoise       float64 `json:"kalman_measurement_noise"`         // 卡尔曼滤波器测量噪声协方差 R
	KalmanWindowSize             int     `json:"kalman_window_size"`               // 卡尔曼滤波器窗口大小
}

// DefaultAutoBrightnessConfig 默认自动亮度配置
var DefaultAutoBrightnessConfig = AutoBrightnessConfig{
	Enabled:                      false,
	Sensitivity:                  0.5,
	PollingInterval:              5,
	ChangeThreshold:              20.0,
	BrightnessChangeThreshold:    0.01,
	ManualOverrideDuration:       300,
	ManualAdjustDisablesAutoMode: true,
	UseTransition:                true,
	KalmanProcessNoise:           0.8,
	KalmanMeasurementNoise:       0.05,
	KalmanWindowSize:             3,
}

// DSettings键名已在manager.go中定义
// AutoBrightnessManager 自动亮度管理器
type Manager struct {
	sensorClient *SensorProxyClient

	config        AutoBrightnessConfig
	configManager configManager.Manager
	sysBus        *dbus.Conn
	dbusDaemon    ofdbus.DBus
	sysSigLoop    *dbusutil.SignalLoop

	enabled        bool
	supported      bool
	manualOverride time.Time
	lastLightLevel int
	lastAdjustTime time.Time
	lastBrightness float64

	kalmanFilter *brightness.AdaptiveKalmanFilter

	systemAdjusting bool
	running         bool

	// 亮度设置回调，由外部注入
	setBrightnessFunc func(value float64) error
	// 自动亮度被禁用时的回调
	onAutoBrightnessDisabled func()
	// 是否使用渐变效果
	useTransition bool

	lastSensorDataTime time.Time
	compensationTicker *time.Ticker
	compensationStopCh chan struct{}
	handler            interface{}
	eventChannel       chan AutoBrightnessEvent

	// 重试和降级配置
	maxRetries          int
	retryInterval       time.Duration
	gracefulDegradation bool
}

func NewManager(configManager configManager.Manager, sysBus *dbus.Conn) *Manager {
	return &Manager{
		lastLightLevel:      -1,
		lastBrightness:      -1,
		maxRetries:          3,
		retryInterval:       time.Second * 2,
		gracefulDegradation: true,
		eventChannel:        make(chan AutoBrightnessEvent, 10),
	}
}

// SetBrightnessSetter 设置亮度回调函数
func (m *Manager) SetBrightnessSetter(setter func(value float64) error) {
	m.setBrightnessFunc = setter
}

// SetOnAutoBrightnessDisabled 设置自动亮度被禁用时的回调
func (m *Manager) SetOnAutoBrightnessDisabled(callback func()) {
	m.onAutoBrightnessDisabled = callback
}

func (m *Manager) Init() error {
	sysBus, err := dbus.SystemBus()
	if err != nil {
		return err
	}
	m.sysBus = sysBus
	m.dbusDaemon = ofdbus.NewDBus(m.sysBus)
	m.sysSigLoop = dbusutil.NewSignalLoop(m.sysBus, 10)
	m.sysSigLoop.Start()
	m.dbusDaemon.InitSignalExt(m.sysSigLoop, true)
	sensorProxy := sensorproxy.NewSensorProxy(sysBus)
	sensorClient := NewSensorProxyClient(sensorProxy, m.dbusDaemon)
	m.sensorClient = sensorClient

	err = m.checkSensorAvailability()
	if err != nil {
		m.supported = false
		logger.Warning("[AutoBrightness] Sensor not available:", err)
		return fmt.Errorf("sensor not available: %w", err)
	}

	sensorClient.SetServiceChangeCallback(m.onServiceChange)
	sensorClient.SetLightLevelChangeCallback(m.onLightLevelChange)

	config, err := m.getConfig()
	if err != nil {
		logger.Warning("[AutoBrightness] Failed to load config, using default:", err)
		config = DefaultAutoBrightnessConfig
	}
	m.config = config
	m.useTransition = config.UseTransition
	m.supported = true
	m.applyKalmanFilterConfig()

	m.startCompensationTimer()

	logger.Info("[AutoBrightness] AutoBrightnessManager initialized successfully")
	return nil
}

func (m *Manager) startCompensationTimer() {
	m.compensationTicker = time.NewTicker(compensationInterval)
	m.compensationStopCh = make(chan struct{})

	go func() {
		for {
			select {
			case <-m.compensationTicker.C:
				ch := m.eventChannel
				if ch == nil {
					return
				}
				event := CompensationLightEvent{CompensationLightLevel: m.lastLightLevel}
				ch <- event
			case <-m.compensationStopCh:
				return
			}
		}
	}()
}

func (m *Manager) stopCompensationTimer() {
	if m.compensationTicker == nil {
		return
	}
	m.compensationTicker.Stop()
	m.compensationTicker = nil
	close(m.compensationStopCh)
	m.compensationStopCh = nil
}

func (m *Manager) Run() error {
	for ev := range m.eventChannel {
		if _, ok := ev.(StopEvent); ok {
			ev.Handle(m)
			return nil
		}
		ev.Handle(m)
	}
	return nil
}

func (m *Manager) Stop() error {
	logger.Info("[AutoBrightness] Stopping auto brightness manager")

	// 停止补偿定时器
	m.stopCompensationTimer()

	// 断开传感器连接（会自动释放 sensor）
	if m.sensorClient != nil {
		err := m.sensorClient.Disconnect()
		if err != nil {
			logger.Warning("[AutoBrightness] Failed to disconnect sensor:", err)
		}
	}

	// 停止信号循环
	if m.sysSigLoop != nil {
		m.sysSigLoop.Stop()
	}

	// 关闭事件通道，使 Run() 退出
	if m.eventChannel != nil {
		close(m.eventChannel)
		m.eventChannel = nil
	}

	// 重置状态
	m.running = false
	m.enabled = false
	m.lastLightLevel = -1
	m.lastBrightness = -1
	m.lastAdjustTime = time.Time{}
	m.lastSensorDataTime = time.Time{}

	return nil
}

// checkSensorAvailability 检查传感器是否可用（不连接）
func (m *Manager) checkSensorAvailability() error {
	// 临时连接以检查传感器
	err := m.sensorClient.Connect(m.sysSigLoop)
	if err != nil {
		return fmt.Errorf("failed to connect to sensor proxy: %w", err)
	}
	// 检查完成后立即断开连接
	// 实际使用时会在Start()中重新连接
	defer m.sensorClient.Disconnect()
	// 检查是否有环境光传感器
	hasLight, err := m.sensorClient.HasAmbientLight()
	if err != nil {
		return fmt.Errorf("failed to check ambient light sensor: %w", err)
	}
	if !hasLight {
		return errors.New("no ambient light sensor available")
	}
	return nil
}

// onServiceChange 服务状态变化回调
func (m *Manager) onServiceChange(available bool) {
	ch := m.eventChannel
	if ch == nil {
		return
	}
	newEvent := ServiceChangeEvent{ServiceName: "SensorProxy"}
	ch <- newEvent
}

// onLightLevelChange 光照值变化回调（推送模式）
func (m *Manager) onLightLevelChange(rawLightLevel int) {
	ch := m.eventChannel
	if ch == nil {
		return
	}
	newEvent := LightLevelChangeEvent{RawLightLevel: rawLightLevel}
	ch <- newEvent
}

// getConfig 获取自动亮度配置
func (m *Manager) getConfig() (AutoBrightnessConfig, error) {
	if m.configManager == nil {
		logger.Warning("[AutoBrightness] Config manager is nil, using default config")
		return DefaultAutoBrightnessConfig, nil
	}
	var config AutoBrightnessConfig
	// Enabled
	if val, err := m.configManager.Value(0, DSettingsKeyABEnabled); err == nil {
		if b, ok := val.Value().(bool); ok {
			config.Enabled = b
		} else {
			config.Enabled = DefaultAutoBrightnessConfig.Enabled
		}
	} else {
		config.Enabled = DefaultAutoBrightnessConfig.Enabled
	}
	// Sensitivity
	if val, err := m.configManager.Value(0, DSettingsKeyABSensitivity); err == nil {
		switch v := val.Value().(type) {
		case float64:
			config.Sensitivity = v
		case int64:
			config.Sensitivity = float64(v)
		default:
			logger.Warningf("[AutoBrightness] Invalid type for sensitivity: %T", v)
			config.Sensitivity = DefaultAutoBrightnessConfig.Sensitivity
		}
	} else {
		logger.Warning("[AutoBrightness] Config convert faild, using default sensitivity")
		config.Sensitivity = DefaultAutoBrightnessConfig.Sensitivity
	}
	// ChangeThreshold
	if val, err := m.configManager.Value(0, DSettingsKeyABChangeThreshold); err == nil {
		switch v := val.Value().(type) {
		case float64:
			config.ChangeThreshold = v
		case int64:
			config.ChangeThreshold = float64(v)
		default:
			logger.Warningf("[AutoBrightness] Invalid type for changeThreshold: %T", v)
			config.ChangeThreshold = DefaultAutoBrightnessConfig.ChangeThreshold
		}
	} else {
		logger.Warning("[AutoBrightness] Config convert faild, using default changeThreshold")
		config.ChangeThreshold = DefaultAutoBrightnessConfig.ChangeThreshold
	}

	// BrightnessChangeThreshold
	if val, err := m.configManager.Value(0, DSettingsKeyABBrightnessChangeThreshold); err == nil {
		config.BrightnessChangeThreshold = val.Value().(float64)
	} else {
		logger.Warning("[AutoBrightness] Config convert faild, using default brightnessChangeThreshold")
		config.BrightnessChangeThreshold = DefaultAutoBrightnessConfig.BrightnessChangeThreshold
	}

	// PollingInterval
	if val, err := m.configManager.Value(0, DSettingsKeyABPollingInterval); err == nil {
		switch v := val.Value().(type) {
		case int64:
			config.PollingInterval = int(v)
		case float64:
			config.PollingInterval = int(v)
		default:
			config.PollingInterval = DefaultAutoBrightnessConfig.PollingInterval
		}
	} else {
		logger.Warning("[AutoBrightness] Config convert faild, using default pollingInterval")
		config.PollingInterval = DefaultAutoBrightnessConfig.PollingInterval
	}
	// ManualOverrideDuration
	if val, err := m.configManager.Value(0, DSettingsKeyABManualOverride); err == nil {
		switch v := val.Value().(type) {
		case int64:
			config.ManualOverrideDuration = int(v)
		case float64:
			config.ManualOverrideDuration = int(v)
		default:
			config.ManualOverrideDuration = DefaultAutoBrightnessConfig.ManualOverrideDuration
		}
	} else {
		logger.Warning("[AutoBrightness] Config convert faild, using default manualOverrideDuration")
		config.ManualOverrideDuration = DefaultAutoBrightnessConfig.ManualOverrideDuration
	}
	// ManualAdjustDisablesAutoMode
	if val, err := m.configManager.Value(0, DSettingsKeyABManualAdjustDisablesAutoMode); err == nil {
		if b, ok := val.Value().(bool); ok {
			config.ManualAdjustDisablesAutoMode = b
		} else {
			config.ManualAdjustDisablesAutoMode = DefaultAutoBrightnessConfig.ManualAdjustDisablesAutoMode
		}
	} else {
		logger.Warning("[AutoBrightness] Config convert faild, using default manualAdjustDisablesAutoMode")
		config.ManualAdjustDisablesAutoMode = DefaultAutoBrightnessConfig.ManualAdjustDisablesAutoMode
	}
	// UseTransition
	if val, err := m.configManager.Value(0, DSettingsKeyABUseTransition); err == nil {
		if b, ok := val.Value().(bool); ok {
			config.UseTransition = b
		} else {
			config.UseTransition = DefaultAutoBrightnessConfig.UseTransition
		}
	} else {
		logger.Warning("[AutoBrightness] Config convert faild, using default useTransition")
		config.UseTransition = DefaultAutoBrightnessConfig.UseTransition
	}

	// Curve
	if val, err := m.configManager.Value(0, DSettingsKeyABCurve); err == nil {
		itemList, ok := val.Value().([]dbus.Variant)
		if ok && len(itemList) > 0 {
			var points []brightness.AutoBrightnessCurvePoint
			for _, item := range itemList {
				pointMap, ok := item.Value().(map[string]dbus.Variant)
				if !ok {
					continue
				}
				var point brightness.AutoBrightnessCurvePoint
				if luxVal, ok := pointMap["lux"]; ok {
					switch v := luxVal.Value().(type) {
					case int64:
						point.Lux = int(v)
					case float64:
						point.Lux = int(v)
					}
				}
				if brVal, ok := pointMap["br"]; ok {
					switch v := brVal.Value().(type) {
					case int64:
						point.Br = float64(v)
					case float64:
						point.Br = v
					}
				}
				points = append(points, point)
			}
			brightness.SetAutoBrightnessCurveFromPoints(points)
		} else {
			logger.Debug("[AutoBrightness] Curve config is empty, using default linear mapping")
		}
	} else {
		logger.Debug("[AutoBrightness] No curve config found, using default linear mapping")
	}

	// KalmanProcessNoise
	if val, err := m.configManager.Value(0, DSettingsKeyABKalmanProcessNoise); err == nil {
		config.KalmanProcessNoise = val.Value().(float64)
	} else {
		logger.Warning("[AutoBrightness] Config convert failed, using default kalmanProcessNoise")
		config.KalmanProcessNoise = DefaultAutoBrightnessConfig.KalmanProcessNoise
	}

	// KalmanMeasurementNoise
	if val, err := m.configManager.Value(0, DSettingsKeyABKalmanMeasurementNoise); err == nil {
		config.KalmanMeasurementNoise = val.Value().(float64)
	} else {
		logger.Warning("[AutoBrightness] Config convert failed, using default kalmanMeasurementNoise")
		config.KalmanMeasurementNoise = DefaultAutoBrightnessConfig.KalmanMeasurementNoise
	}

	// KalmanWindowSize
	if val, err := m.configManager.Value(0, DSettingsKeyABKalmanWindowSize); err == nil {
		switch v := val.Value().(type) {
		case int64:
			config.KalmanWindowSize = int(v)
		case float64:
			config.KalmanWindowSize = int(v)
		default:
			config.KalmanWindowSize = DefaultAutoBrightnessConfig.KalmanWindowSize
		}
	} else {
		logger.Warning("[AutoBrightness] Config convert failed, using default kalmanWindowSize")
		config.KalmanWindowSize = DefaultAutoBrightnessConfig.KalmanWindowSize
	}

	// 验证配置有效性
	logger.Debugf("[AutoBrightness] Apply config: %v", config)
	err := config.Validate()
	if err != nil {
		logger.Warning("[AutoBrightness] Invalid config, using default:", err)
		return DefaultAutoBrightnessConfig, nil
	}
	return config, nil
}

func setGlobalDconfValue(appID string, name string, subPath string, key string, value dbus.Variant) error {
	sysBus, err := dbus.SystemBus()
	if err != nil {
		logger.Warning(err)
		return err
	}
	ds := configManager.NewConfigManager(sysBus)
	managerPath, err := ds.AcquireManager(0, appID, name, subPath)
	if err != nil {
		logger.Warning(err)
		return err
	}

	dsManager, err := configManager.NewManager(sysBus, managerPath)
	if err != nil {
		logger.Warning(err)
		return err
	}

	err = dsManager.SetValue(0, key, value)
	if err != nil {
		logger.Warning(err)
		return err
	}
	return nil
}

// saveConfig 保存自动亮度配置
func (m *Manager) saveConfig() error {
	if m.configManager == nil {
		return errors.New("config manager is nil")
	}
	// 验证配置有效性
	err := m.config.Validate()
	if err != nil {
		return err
	}
	// 保存各个配置项
	err = setGlobalDconfValue(DSettingsAutoBrightnessAppID, DSettingsAutoBrightnessName, "",
		DSettingsKeyABEnabled, dbus.MakeVariant(m.config.Enabled))
	if err != nil {
		return err
	}
	err = setGlobalDconfValue(DSettingsAutoBrightnessAppID, DSettingsAutoBrightnessName, "",
		DSettingsKeyABSensitivity, dbus.MakeVariant(m.config.Sensitivity))
	if err != nil {
		return err
	}
	err = setGlobalDconfValue(DSettingsAutoBrightnessAppID, DSettingsAutoBrightnessName, "",
		DSettingsKeyABChangeThreshold, dbus.MakeVariant(m.config.ChangeThreshold))
	if err != nil {
		return err
	}
	err = setGlobalDconfValue(DSettingsAutoBrightnessAppID, DSettingsAutoBrightnessName, "",
		DSettingsKeyABPollingInterval, dbus.MakeVariant(m.config.PollingInterval))
	if err != nil {
		return err
	}
	err = setGlobalDconfValue(DSettingsAutoBrightnessAppID, DSettingsAutoBrightnessName, "",
		DSettingsKeyABManualOverride, dbus.MakeVariant(m.config.ManualOverrideDuration))
	if err != nil {
		return err
	}
	err = setGlobalDconfValue(DSettingsAutoBrightnessAppID, DSettingsAutoBrightnessName, "",
		DSettingsKeyABManualAdjustDisablesAutoMode, dbus.MakeVariant(m.config.ManualAdjustDisablesAutoMode))
	if err != nil {
		return err
	}
	err = setGlobalDconfValue(DSettingsAutoBrightnessAppID, DSettingsAutoBrightnessName, "",
		DSettingsKeyABUseTransition, dbus.MakeVariant(m.config.UseTransition))
	if err != nil {
		return err
	}
	return nil
}

// applyKalmanFilterConfig 应用卡尔曼滤波器配置
func (m *Manager) applyKalmanFilterConfig() {
	if m.kalmanFilter == nil {
		m.kalmanFilter = brightness.NewAdaptiveKalmanFilter(
			m.config.KalmanProcessNoise,
			m.config.KalmanMeasurementNoise,
			m.config.KalmanWindowSize,
		)
		logger.Debug("[AutoBrightness] Kalman filter created with config params")
		return
	}

	m.kalmanFilter.SetProcessNoise(m.config.KalmanProcessNoise)
	m.kalmanFilter.SetMeasurementNoise(m.config.KalmanMeasurementNoise)
	m.kalmanFilter.SetWindowSize(m.config.KalmanWindowSize)
	logger.Debugf("[AutoBrightness] Kalman filter params updated: Q=%.4f, R=%.4f, window=%d",
		m.config.KalmanProcessNoise, m.config.KalmanMeasurementNoise, m.config.KalmanWindowSize)
}

// 配置管理方法
// Validate 验证配置参数的有效性
func (config *AutoBrightnessConfig) Validate() error {
	if config.Sensitivity < 0.1 {
		return errors.New("sensitivity too small")
	}
	if config.PollingInterval < 1 {
		return errors.New("polling interval too small")
	}
	if config.ChangeThreshold < 1.0 {
		return errors.New("change threshold too small")
	}
	if config.ManualOverrideDuration < 1 {
		return errors.New("manual override duration too small")
	}
	if config.KalmanProcessNoise <= 0 || config.KalmanProcessNoise > 100 {
		return errors.New("kalman process noise must be positive and less than 100")
	}
	if config.KalmanMeasurementNoise <= 0 || config.KalmanMeasurementNoise > 100 {
		return errors.New("kalman measurement noise must be positive and less than 100")
	}
	if config.KalmanWindowSize < 2 || config.KalmanWindowSize > 100 {
		return errors.New("kalman window size must be between 2 and 100")
	}
	return nil
}
