// SPDX-FileCopyrightText: 2018 - 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later

package audio

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	dbus "github.com/godbus/dbus/v5"
	notifications "github.com/linuxdeepin/go-dbus-factory/session/org.freedesktop.notifications"
	"github.com/linuxdeepin/go-lib/dbusutil"
	"github.com/linuxdeepin/go-lib/gettext"
	"github.com/linuxdeepin/go-lib/pulse"
)

// 一次性读出所有事件
func (a *Audio) pollEvents() []*pulse.Event {
	events := make([]*pulse.Event, 0)

FOR:
	for {
		select {
		case event := <-a.eventChan:
			events = append(events, event)
		default:
			logger.Debugf("poll %d events", len(events))
			break FOR
		}
	}

	return events
}

// 事件分发
func (a *Audio) dispatchEvents(events []*pulse.Event) {
	logger.Debugf("dispatch %d events", len(events))
	for i, event := range events {
		logger.Debugf("dispatch %dth event:facility<%d> type<%d> index<%d>", i, event.Facility, event.Type, event.Index)
		switch event.Facility {
		case pulse.FacilityServer:
			a.handleServerEvent(event.Type)
		case pulse.FacilityCard:
			a.handleCardEvent(event.Type, event.Index)
			a.saveConfig()
		case pulse.FacilitySink:
			a.handleSinkEvent(event.Type, event.Index)
			a.saveConfig()
		case pulse.FacilitySource:
			a.handleSourceEvent(event.Type, event.Index)
			a.saveConfig()
		case pulse.FacilitySinkInput:
			a.handleSinkInputEvent(event.Type, event.Index)
			a.saveConfig()
		}
	}
	logger.Debug("dispatch events done")
}

func (a *Audio) handleEvent() {
	for {
		select {
		case event := <-a.eventChan:
			tail := a.pollEvents()
			events := make([]*pulse.Event, 0, 1+len(tail))
			events = append(events, event)
			events = append(events, tail...)
			a.dispatchEvents(events)

		case <-a.quit:
			logger.Debug("handleEvent return")
			return
		}
	}
}

func (a *Audio) handleStateChanged() {
	for {
		select {
		case state := <-a.stateChan:
			switch state {
			case pulse.ContextStateFailed:
				logger.Warning("pulseaudio context state failed")
				a.destroyCtxRelated()

				if !a.noRestartPulseAudio {
					logger.Debug("retry init")
					err := a.init()
					if err != nil {
						logger.Warning("failed to init:", err)
					}
					return
				} else {
					logger.Debug("do not restart pulseaudio")
				}
			}

		case <-a.quit:
			logger.Debug("handleStateChanged return")
			return
		}
	}
}

func (a *Audio) isCardIdValid(cardId uint32) bool {
	for _, card := range a.cards {
		if card.Id == cardId {
			return true
		}
	}
	return false
}

func (a *Audio) needAutoSwitchInputPort() bool {
	// 不支持自动切换端口
	if !a.canAutoSwitchPort() {
		return false
	}

	firstPort, _ := GetPriorityManager().GetTheFirstPort(pulse.DirectionSource)

	// 没有可用端口
	if firstPort == nil || firstPort.PortType == PortTypeInvalid {
		logger.Debug("no input port")
		return false
	}

	// 检查当前profile是否是配置文件中设置的profile
	card, err := a.cards.getByName(firstPort.CardName)
	if err != nil {
		logger.Warning(err)
		return false
	}
	cp := card.core.ActiveProfile
	port, err := card.Ports.Get(firstPort.PortName, pulse.DirectionSource)
	if err != nil {
		logger.Warning(err)
		return false
	}

	// 输入端口不应该主动切换配置文件，会导致输入端口不可用或者发生变化
	// 如果当前端口和当前配置文件匹配，不需要切换端口
	if port.Profiles.Exists(cp.Name) {
		return false
	}

	// 同端口切换次数超出限制(切换失败时反复切换同一端口)
	if a.inputAutoSwitchCount >= 10 &&
		(a.inputCardName == firstPort.CardName && a.inputPortName == firstPort.PortName) {
		logger.Debug("input auto switch tried too many times")
		return false
	}

	// 当前端口就是优先级最高的端口
	var currentCardName, currentPortName string
	if a.defaultSource != nil {
		currentCardName = a.getCardNameById(a.defaultSource.Card)
		currentPortName = a.defaultSource.ActivePort.Name
	}

	if currentCardName == firstPort.CardName && currentPortName == firstPort.PortName {
		logger.Debugf("current input<%s,%s> is already the first port",
			currentCardName, currentPortName)
		return false
	}

	logger.Debugf("will auto switch from input<%s,%s> to input<%s,%s>",
		currentCardName, currentPortName, firstPort.CardName, firstPort.PortName)
	return true
}

func (a *Audio) autoSwitchOutputPort() error {
	// 不支持自动切换端口
	if !a.canAutoSwitchPort() {
		return nil
	}

	var currentCardName, currentPortName string
	if a.defaultSink != nil {
		currentCardName = a.getCardNameById(a.defaultSink.Card)
		currentPortName = a.defaultSink.ActivePort.Name
	}
	logger.Warning("handle autoSwitchOutputPort")
	prefer, pos := GetPriorityManager().GetTheFirstPort(pulse.DirectionSink)
	if pos.tp != PortTypeInvalid {
		logger.Warning("loop prefer port:", *prefer)
		card, err := a.cards.getByName(prefer.CardName)
		if err != nil {
			logger.Warning(err)
			return nil
		}
		logger.Debugf("will auto switch from input<%s,%s> to input<%s,%s>",
			currentCardName, currentPortName, prefer.CardName, prefer.PortName)
		return a.setPort(card.Id, prefer.PortName, pulse.DirectionSink, true)

	}
	return nil
}

func (a *Audio) autoSwitchInputPort() error {
	// 不支持自动切换端口
	if !a.canAutoSwitchPort() {
		return nil
	}

	// 当前端口就是优先级最高的端口
	var currentCardName, currentPortName string
	if a.defaultSource != nil {
		currentCardName = a.getCardNameById(a.defaultSource.Card)
		currentPortName = a.defaultSource.ActivePort.Name
	}
	prefer, pos := GetPriorityManager().GetTheFirstPort(pulse.DirectionSource)
	for pos.tp != PortTypeInvalid {
		logger.Warning("loop prefer port:", *prefer)
		card, err := a.cards.getByName(prefer.CardName)
		if err != nil {
			logger.Warning(err)
			return nil
		}
		port, err := card.Ports.Get(prefer.PortName, pulse.DirectionSource)
		if err != nil {
			logger.Warning(err)
			return nil
		}
		if port.Profiles.Exists(card.ActiveProfile.Name) {
			logger.Debugf("will auto switch from input<%s,%s> to input<%s,%s>",
				currentCardName, currentPortName, prefer.CardName, prefer.PortName)
			return a.setPort(card.Id, prefer.PortName, pulse.DirectionSource, true)
		}
		prefer, pos = GetPriorityManager().GetNextPort(pulse.DirectionSource, pos)
		if prefer == nil {
			return errors.New("no input port")
		}
	}
	return nil
}

func (a *Audio) needAutoSwitchOutputPort() bool {
	// 不支持自动切换端口
	if !a.canAutoSwitchPort() {
		return false
	}
	logger.Debug("check need auto switch output")
	firstPort, _ := GetPriorityManager().GetTheFirstPort(pulse.DirectionSink)

	// 没有可用端口
	if firstPort == nil || firstPort.PortType == PortTypeInvalid {
		logger.Debug("no output port")
		return false
	}

	// 检查当前profile是否是配置文件中设置的profile
	card, err := a.cards.getByName(firstPort.CardName)
	if err != nil {
		logger.Warning(err)
		return false
	}
	cp := card.core.ActiveProfile
	profile := GetConfigKeeper().GetMode(firstPort.CardName, firstPort.PortName)
	if profile != "" && cp.Name != profile {
		logger.Warningf("output profile not match, current: %s, prefer: %s", cp.Name, profile)
		return true
	}

	// 当前端口就是优先级最高的端口
	var currentCardName, currentPortName string
	if a.defaultSink != nil {
		currentCardName = a.getCardNameById(a.defaultSink.Card)
		currentPortName = a.defaultSink.ActivePort.Name
	}

	if currentCardName == firstPort.CardName && currentPortName == firstPort.PortName {
		logger.Debugf("current output<%s,%s> is already the first",
			currentCardName, currentPortName)
		return false
	}

	logger.Debugf("will auto switch from output<%s,%s> to output<%s,%s>",
		currentCardName, currentPortName, firstPort.CardName, firstPort.PortName)
	return true
}

// 自动切换端口，至少要保证声卡的profile是配置文件中设置的profile
// 如果不是，可能还在切换中，等待一下
func (a *Audio) autoSwitchPort() {
	logger.Warning("auto switch port")
	if err := a.autoSwitchOutputPort(); err != nil {
		logger.Warning(err)
	}
	if err := a.autoSwitchInputPort(); err != nil {
		logger.Warning(err)
	}
}

func (a *Audio) handleCardEvent(eventType int, idx uint32) {
	var shouldAutoSwitch = false
	switch eventType {
	case pulse.EventTypeNew: // 新增声卡
		a.autoPause()
		a.handleCardAdded(idx)
		shouldAutoSwitch = true
	case pulse.EventTypeRemove: // 删除声卡
		a.autoPause()
		a.handleCardRemoved(idx)
		shouldAutoSwitch = true
	case pulse.EventTypeChange: // 声卡属性变化,也可能是有线耳机插拔了端口
		shouldAutoSwitch = a.handleCardChanged(idx)
	default:
		logger.Warningf("unhandled card event, card=%d, type=%d", idx, eventType)
	}

	// 保存旧的cards
	a.oldCards = a.cards
	if shouldAutoSwitch {
		logger.Warning("refresh card...")
		GetPriorityManager().refreshPorts(a.cards)
		GetPriorityManager().Save()
		if a.checkCardIsReady(idx) {
			a.autoSwitchPort()
		}
	}
}

func (a *Audio) handleCardAdded(idx uint32) {
	// 数据更新在refreshCards中统一处理，这里只做业务逻辑上的响应
	logger.Debugf("card %d added", idx)
	card, err := a.ctx.GetCard(idx)
	if err != nil {
		logger.Warning(err)
		return
	}
	ac := newCard(card)
	a.cards = append(a.cards, ac)
	cards := a.cards.string()
	a.setPropCards(cards)
	a.setPropCardsWithoutUnavailable(a.cards.stringWithoutUnavailable())

	// 这里写所有类型的card事件都需要触发的逻辑
	/* 新增声卡上的端口如果被处于禁用状态，进行横幅提示 */
	if isBluezAudio(card.Name) {
		logger.Debugf("notify bluez card %s", card.Name)
		a.notifyBluezCardPortInsert(ac)
	} else {
		logger.Debugf("notify normal card %s", card.Name)
		a.notifyCardPortInsert(ac)
	}
}

func (a *Audio) handleCardRemoved(idx uint32) {
	// 数据更新在refreshCards中统一处理，这里只做业务逻辑上的响应
	// 注意，此时idx已经失效了，无法获取已经失去的数据，如果业务需要，应当在refresh前进行数据备份
	logger.Debugf("card %d removed", idx)
	a.cards, _ = a.cards.delete(idx)
	cards := a.cards.string()
	a.setPropCards(cards)
	a.setPropCardsWithoutUnavailable(a.cards.stringWithoutUnavailable())
}

func (a *Audio) handleCardChanged(idx uint32) bool {
	// 数据更新在refreshSinks中统一处理，这里只做业务逻辑上的响应
	logger.Debugf("card %d changed", idx)
	pc, err := a.ctx.GetCard(idx)
	if err != nil {
		logger.Warning(err)
		return false
	}
	ac, err := a.cards.get(idx)
	if err != nil {
		logger.Warningf("invalid card index #%d", idx)
		return false
	}

	ac.core = pc
	ac.update(pc)

	cards := a.cards.string()
	a.setPropCards(cards)
	a.setPropCardsWithoutUnavailable(a.cards.stringWithoutUnavailable())

	oldCard, err := a.oldCards.get(ac.Id)
	if err != nil && oldCard != nil {
		return ac.doDiff(oldCard) == NoChange
	}
	return false
}

func (a *Audio) handleSinkEvent(eventType int, idx uint32) {
	switch eventType {
	case pulse.EventTypeNew: // 新增sink
		a.handleSinkAdded(idx)
	case pulse.EventTypeRemove: // 删除sink
		a.handleSinkRemoved(idx)
	case pulse.EventTypeChange: // sink属性变化
		a.handleSinkChanged(idx)
	default:
		logger.Warningf("unhandled sink event, sink=%d, type=%d", idx, eventType)
	}

}

func (a *Audio) handleSinkAdded(idx uint32) {
	// 数据更新在refreshSinks中统一处理，这里只做业务逻辑上的响应
	sink, err := a.ctx.GetSink(idx)
	if err != nil {
		logger.Warning(err)
		return
	}
	logger.Debugf("sink %d %s added", idx, sink.Name)
	if sink.Name == dndVirtualSinkName {
		port := pulse.PortInfo{
			Name:        sink.Name,
			Description: dndVirtualSinkDescription,
			Priority:    0,
			Available:   2,
		}
		sink.Ports = append(sink.Ports, port)
		sink.ActivePort = port
	}

	if _, exist := a.sinks[idx]; exist {
		a.sinks[idx].update(sink)
	} else {
		a.addSink(sink)
	}

	if !isPhysicalDevice(sink.Name) {
		if sink.Name == monoSinkName && a.Mono {
			logger.Debug("set mono as default sink")
			a.ctx.SetDefaultSink(monoSinkName)
		}
	} else {
		if a.checkCardIsReady(sink.Card) {
			a.autoSwitchPort()
		}
	}
}

func (a *Audio) handleSinkRemoved(idx uint32) {
	// 数据更新在refreshSinks中统一处理，这里只做业务逻辑上的响应
	// 注意，此时idx已经失效了，无法获取已经失去的数据，如果业务需要，应当在refresh前进行数据备份
	var cardId uint32
	var isPhy bool
	logger.Debugf("sink %d removed", idx)
	if sink, exist := a.sinks[idx]; exist {
		cardId = sink.Card
		isPhy = isPhysicalDevice(sink.Name)
		a.service.StopExport(a.sinks[idx])
		delete(a.sinks, idx)
	}
	a.updatePropSinks()
	if a.defaultSink != nil && a.defaultSink.index == idx {
		logger.Debugf("set default sink to / because of sink removed")
		a.setPropDefaultSink("/")
		a.defaultSinkName = ""
		a.defaultSink = nil
	} else {
		return
	}
	if isPhy && a.checkCardIsReady(cardId) {
		a.autoSwitchPort()
	}
}

func (a *Audio) handleSinkChanged(idx uint32) {
	// 数据更新在refreshSinks中统一处理，这里只做业务逻辑上的响应
	logger.Debugf("sink %d changed", idx)
	sink, err := a.ctx.GetSink(idx)
	if err != nil {
		logger.Warning(err)
		return
	}
	if _, ok := a.sinks[idx]; ok {
		a.sinks[idx].update(sink)
	}

}

func (a *Audio) handleSourceEvent(eventType int, idx uint32) {
	switch eventType {
	case pulse.EventTypeNew:
		a.handleSourceAdded(idx)
	case pulse.EventTypeRemove:
		a.handleSourceRemoved(idx)
	case pulse.EventTypeChange:
		a.handleSourceChanged(idx)
	default:
		logger.Warningf("unhandled source event, sink=%d, type=%d", idx, eventType)
	}
}

func (a *Audio) handleSourceAdded(idx uint32) {
	// 数据更新在refreshSources中统一处理，这里只做业务逻辑上的响应
	logger.Debugf("source %d added", idx)
	source, err := a.ctx.GetSource(idx)
	if err != nil {
		logger.Warning(err)
		return
	}
	if strings.HasSuffix(source.Name, ".monitor") {
		logger.Debugf("skip %s source update", source.Name)
		return
	}
	if _, exist := a.sources[idx]; exist {
		a.sources[idx].update(source)
	} else {
		a.addSource(source)
	}
	a.updatePropSources()

	if isPhysicalDevice(source.Name) && a.checkCardIsReady(source.Card) {
		a.autoSwitchPort()
	}
}

func (a *Audio) handleSourceRemoved(idx uint32) {
	// 数据更新在refreshSources中统一处理，这里只做业务逻辑上的响应
	// 注意，此时idx已经失效了，无法获取已经失去的数据，如果业务需要，应当在refresh前进行数据备份
	logger.Debugf("source %d removed", idx)
	var cardId uint32
	var isPhy bool
	if source, exist := a.sources[idx]; exist {
		cardId = source.Card
		isPhy = isPhysicalDevice(source.Name)
		a.service.StopExport(a.sources[idx])
		delete(a.sources, idx)
	} else {
		return
	}
	a.updatePropSources()
	if a.defaultSource != nil && a.defaultSource.index == idx {
		logger.Warning("set default source to / because of source removed")
		a.setPropDefaultSource("/")
		a.defaultSourceName = ""
		a.defaultSource = nil
	}
	if isPhy && a.checkCardIsReady(cardId) {
		a.autoSwitchPort()
	}
}

func (a *Audio) handleSourceChanged(idx uint32) {
	// 数据更新在refreshSources中统一处理，这里只做业务逻辑上的响应
	logger.Debugf("source %d changed", idx)
	source, err := a.ctx.GetSource(idx)
	if err != nil {
		logger.Warning(err)
		return
	}

	if _, ok := a.sources[idx]; ok {
		a.sources[idx].update(source)
	}
}

func (a *Audio) handleSinkInputEvent(eventType int, idx uint32) {
	switch eventType {
	case pulse.EventTypeNew:
		a.handleSinkInputAdded(idx)
	case pulse.EventTypeRemove:
		a.handleSinkInputRemoved(idx)
	case pulse.EventTypeChange:
		a.handleSinkInputChanged(idx)
	default:
		logger.Warningf("unhandled sink-input event, sink-input=%d, type=%d", idx, eventType)
	}

	// 这里写所有类型的sink-input事件都需要触发的逻辑
}

func (a *Audio) handleSinkInputAdded(idx uint32) {
	// 数据更新在refreshSinkInputs中统一处理，这里只做业务逻辑上的响应
	sinkInput, err := a.ctx.GetSinkInput(idx)
	if err != nil {
		logger.Warning(err)
		return
	}
	logger.Debugf("sink-input %d %s added", idx, sinkInput.Name)
	if _, exist := a.sinkInputs[idx]; exist {
		a.sinkInputs[idx].update(sinkInput)
	} else {
		a.addSinkInput(sinkInput)
	}
	if a.defaultSink != nil {
		list := []uint32{idx}
		logger.Infof("move sink-input %d to default sink", idx)
		a.moveSinkInputsToSink(list)
	}
}

func (a *Audio) handleSinkInputRemoved(idx uint32) {
	// 数据更新在refreshSinkInputs中统一处理，这里只做业务逻辑上的响应
	// 注意，此时idx已经失效了，无法获取已经失去的数据，如果业务需要，应当在refresh前进行数据备份
	logger.Debugf("sink-input %d removed", idx)
	if _, exist := a.sinkInputs[idx]; exist {
		a.service.StopExport(a.sinkInputs[idx])
		delete(a.sinkInputs, idx)
	}
}

func (a *Audio) handleSinkInputChanged(idx uint32) {
	// 数据更新在refreshSinkInputs中统一处理，这里只做业务逻辑上的响应
	logger.Debugf("sink-input %d changed", idx)
	sinkInput, err := a.ctx.GetSinkInput(idx)
	if err != nil {
		logger.Warning(err)
		return
	}

	if _, ok := a.sinkInputs[idx]; ok {
		a.sinkInputs[idx].update(sinkInput)
	}
}

/* 创建开启端口的命令，提供给notification调用 */
func makeNotifyCmdEnablePort(cardId uint32, portName string) string {
	dest := "org.deepin.dde.Audio1"
	path := "/org/deepin/dde/Audio1"
	method := "org.deepin.dde.Audio1.SetPortEnabled"
	return fmt.Sprintf("dbus-send,--type=method_call,--dest=%s,%s,%s,uint32:%d,string:%s,boolean:true",
		dest, path, method, cardId, portName)
}

/* 横幅提示端口被禁用,并提供开启的按钮 */
func notifyPortDisabled(cardId uint32, port pulse.CardPortInfo) {
	session, err := dbus.SessionBus()
	if err != nil {
		logger.Warning(err)
		return
	}

	icon := "disabled-audio-output-plugged"
	if port.Direction == pulse.DirectionSource {
		icon = "disabled-audio-input-plugged"
	}

	cmd := makeNotifyCmdEnablePort(cardId, port.Name)
	message := fmt.Sprintf(gettext.Tr("%s had been disabled"), port.Description)
	actions := []string{"open", gettext.Tr("Open")}
	hints := map[string]dbus.Variant{"x-deepin-action-open": dbus.MakeVariant(cmd)}
	notify := notifications.NewNotifications(session)
	_, err = notify.Notify(
		0,
		gettext.Tr("dde-control-center"),
		0,
		icon,
		message,
		"",
		actions,
		hints,
		15*1000,
	)
	if err != nil {
		logger.Warning(err)
	}

}

func (a *Audio) updateObjPathsProp(type0 string, ids []int, setFn func(value []dbus.ObjectPath) bool) {
	sort.Ints(ids)
	paths := make([]dbus.ObjectPath, len(ids))
	for idx, id := range ids {
		paths[idx] = dbus.ObjectPath(dbusPath + "/" + type0 + strconv.Itoa(id))
	}
	a.PropsMu.Lock()
	setFn(paths)
	a.PropsMu.Unlock()
}

func (a *Audio) updatePropSinks() {
	var ids []int
	a.mu.Lock()
	for _, sink := range a.sinks {
		ids = append(ids, int(sink.index))
	}
	a.mu.Unlock()
	a.updateObjPathsProp("Sink", ids, a.setPropSinks)
}

func (a *Audio) updatePropSources() {
	var ids []int
	a.mu.Lock()
	for _, source := range a.sources {
		ids = append(ids, int(source.index))
	}
	a.mu.Unlock()
	a.updateObjPathsProp("Source", ids, a.setPropSources)
}

func (a *Audio) updatePropSinkInputs() {
	var ids []int
	a.mu.Lock()
	for _, sinkInput := range a.sinkInputs {
		if sinkInput.visible {
			ids = append(ids, int(sinkInput.index))
		}
	}
	a.mu.Unlock()
	a.updateObjPathsProp("SinkInput", ids, a.setPropSinkInputs)
}

func isPhysicalDevice(deviceName string) bool {
	for _, virtualDeviceKey := range []string{
		"echoCancelSource", "echo-cancel", "Echo-Cancel", "remap-sink-mono", // virtual key
	} {
		if strings.Contains(deviceName, virtualDeviceKey) {
			return false
		}
	}
	return true
}

func (a *Audio) handleServerEvent(eventType int) {
	switch eventType {
	case pulse.EventTypeChange:
		server, err := a.ctx.GetServer()
		if err != nil {
			logger.Error(err)
			return
		}
		// defaultSink 和 defaultSource 发生变化，应该只改变dbus属性，不触发自动切换
		// 更新默认sink或者source，这个时候的sink或者source应该已经存在
		a.updateDefaultSink(server.DefaultSinkName)
		a.updateDefaultSource(server.DefaultSourceName)
	}
}

// 外部修改ReducecNoise时触发回调，响应实际降噪开关
func (a *Audio) writeReduceNoise(write *dbusutil.PropertyWrite) *dbus.Error {
	reduce, ok := write.Value.(bool)
	if !ok {
		return dbusutil.ToError(errors.New("type is not bool"))
	}
	if a.ReduceNoise == reduce {
		return nil
	}

	if reduce && a.defaultSource != nil && isBluezAudio(a.defaultSource.Name) {
		logger.Debug("bluetooth audio device cannot open reduce-noise")
		a.ReduceNoise = false
		a.emitPropChangedReduceNoise(a.ReduceNoise)
		return dbusutil.ToError(errors.New("bluetooth audio device cannot open reduce-noise"))
	}

	// 这个配置属性本来应该放在降噪设置成功之后再设置的
	// 但是在开启降噪，切换到降噪的虚拟通道时，需要用对应主设备的配置进行配置恢复
	// 如果不放在前面，配置恢复时，主设备的配置里降噪还处于关闭状态
	// 配置恢复会自动关闭降噪
	source := a.defaultSource
	if source == nil {
		logger.Warning("default source is nil, cannot set reduce noise")
		return dbusutil.ToError(errors.New("default source is nil"))
	}
	GetConfigKeeper().SetReduceNoise(a.getCardNameById(source.Card), source.ActivePort.Name, reduce)
	// 如果取消降噪,先变更defaultsource,再关闭降噪,避免端口发生频繁切换
	a.ctx.SetDefaultSource(a.defaultSource.Name)
	err := a.setReduceNoise(reduce)
	if err != nil {
		logger.Warning("set Reduce Noise failed: ", err)
	}
	a.inputAutoSwitchCount = 0
	return nil
}

func (a *Audio) writeKeyPausePlayer(write *dbusutil.PropertyWrite) *dbus.Error {
	return dbusutil.ToError(a.audioDConfig.SetValue(dsgkeyPausePlayer, write.Value))
}

func (a *Audio) notifyBluezCardPortInsert(card *Card) {
	logger.Debugf("notify bluez card %d:%s", card.Id, card.core.Name)
	oldCard, err := a.oldCards.getByName(card.core.Name)
	if err != nil {
		// oldCard不存在
		logger.Warning(err)
	}

	// 蓝牙会根据模式过滤端口，因此忽略unknown状态
	for _, port := range card.Ports {
		if port.Available == pulse.AvailableTypeNo {
			// 当前状态为AvailableTypeNo，忽略
			logger.Debugf("port %s not insert", port.Name)
			continue
		}

		isInsert := false
		if oldCard == nil {
			// oldCard不存在，即新增声卡
			isInsert = true
		} else {
			oldPort, err := oldCard.getPortByName(port.Name)
			if err != nil {
				// oldPort不存在，例如A2DP切换到headset
				isInsert = true

				// 但是pulseaudio事件时序是乱的，所以可能会因为其它原因进来，导致bug
				logger.Warning(err)
			} else if oldPort.Available == pulse.AvailableTypeNo {
				isInsert = true
			}
		}

		if isInsert {
			logger.Debugf("port<%s,%s> inserted", card.core.Name, port.Name)
			_, portConfig := GetConfigKeeper().GetCardAndPortConfig(card.core.Name, port.Name)
			if !portConfig.Enabled {
				logger.Debugf("port<%s,%s> notify", card.core.Name, port.Name)
				notifyPortDisabled(card.Id, port)
			}
		}
	}
}

func (a *Audio) notifyCardPortInsert(card *Card) {
	logger.Debugf("notify card %d:%s", card.Id, card.core.Name)
	oldCard, err := a.oldCards.getByName(card.core.Name)
	if err != nil {
		// oldCard不存在
		logger.Warning(err)
	}

	for _, port := range card.Ports {
		if port.Available == pulse.AvailableTypeNo {
			// 当前状态为AvailableTypeNo，忽略
			logger.Debugf("port %s not insert", port.Name)
			continue
		}

		isInsert := false
		if oldCard == nil {
			// oldCard不存在，即新增声卡
			isInsert = true
		} else {
			oldPort, err := oldCard.getPortByName(port.Name)
			if err != nil {
				// oldPort不存在，例如A2DP切换到headset
				isInsert = true

				// 但是pulseaudio事件时序是乱的，所以可能会因为其它原因进来，导致bug
				logger.Warning(err)

			} else if oldPort.Available == pulse.AvailableTypeNo {
				logger.Debugf("port %s from AvailableTypeNo to %d", port.Name, port.Available)
				isInsert = true
			} else if oldPort.Available == pulse.AvailableTypeUnknow && port.Available == pulse.AvailableTypeYes {
				logger.Debugf("port %s from AvailableTypeUnknow to AvailableTypeYes", port.Name)
				isInsert = true
			}
		}

		if isInsert {
			logger.Debugf("port<%s,%s> inserted", card.core.Name, port.Name)
			_, portConfig := GetConfigKeeper().GetCardAndPortConfig(card.core.Name, port.Name)
			if !portConfig.Enabled {
				logger.Debugf("port<%s,%s> notify", card.core.Name, port.Name)
				notifyPortDisabled(card.Id, port)
			}
		}
	}
}
