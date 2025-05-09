// SPDX-FileCopyrightText: 2018 - 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later

package audio

import (
	"fmt"
	"sort"

	dbus "github.com/godbus/dbus/v5"
	bluez "github.com/linuxdeepin/go-dbus-factory/system/org.bluez"
	"github.com/linuxdeepin/go-lib/pulse"
	"github.com/linuxdeepin/go-lib/strv"
)

const (
	CardBuildin   = 0
	CardBluethooh = 1
	CardUnknow    = 2

	PropDeviceFromFactor = "device.form_factor"
	PropDeviceBus        = "device.bus"
)

func cardType(c *pulse.Card) int {
	if c.PropList[PropDeviceFromFactor] == "internal" {
		return CardBuildin
	}
	if c.PropList[PropDeviceBus] == "bluetooth" {
		return CardBluethooh
	}
	return CardUnknow
}

func profileBlacklist(c *pulse.Card) strv.Strv {
	var blacklist []string
	switch cardType(c) {
	case CardBluethooh:
		// TODO: bluez not full support headset_head_unit, please skip
		blacklist = []string{"off"}
	default:
		// CardBuildin, CardUnknow and other
		blacklist = []string{"off"}
	}
	return strv.Strv(blacklist)
}

// select New Card Profile By priority, protocl.
func selectNewCardProfile(c *pulse.Card) {
	blacklist := profileBlacklist(c)
	if !blacklist.Contains(c.ActiveProfile.Name) {
		logger.Debug("use profile:", c.ActiveProfile)
		return
	}

	var profiles pulse.ProfileInfos2
	for _, p := range c.Profiles {
		// skip the profile in the blacklist
		if blacklist.Contains(p.Name) {
			continue
		}
		profiles = append(profiles, p)
	}

	// sort profiles by priority
	logger.Debug("[selectNewCardProfile] before sort:", profiles)
	sort.Sort(profiles)
	logger.Debug("[selectNewCardProfile] after sort:", profiles)

	// if card is bluetooth device, switch to profile a2dp_sink
	// only 'a2dp_sink' in bluetooth profiles because of blacklist
	if len(profiles) > 0 {
		if isBluezAudio(c.Name) {
			// Some bluetooth device services not resolved after connected, then denied to set profile to a2dp_sink.
			// If connect device again, the services resolved work right. The devices such as: SONY MDR-1ABT
			if c.ActiveProfile.Name == "off" {
				err := tryConnectBluetooth(c)
				if err != nil {
					logger.Warning("Failed to connect bluetooth card:", c.Name, err)
				}
			}
		}
		logger.Debug("re-select card profile:", profiles[0], c.ActiveProfile.Name)
		if c.ActiveProfile.Name != profiles[0].Name {
			c.SetProfile(profiles[0].Name)
		}
	}
}

func tryConnectBluetooth(c *pulse.Card) error {
	bluePath, ok := c.PropList["bluez.path"]
	if !ok {
		return fmt.Errorf("Not bluetooth card: %s", bluePath)
	}

	logger.Debug("Will try connect bluetooth again:", bluePath)
	conn, err := dbus.SystemBus()
	if err != nil {
		return err
	}
	dev, err := bluez.NewDevice(conn, dbus.ObjectPath(bluePath))
	if err != nil {
		return err
	}
	return dev.Device().Connect(0)
}
