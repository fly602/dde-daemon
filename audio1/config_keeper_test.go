// SPDX-FileCopyrightText: 2018 - 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later
package audio

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigKeeper_Save(t *testing.T) {
	type fields struct {
		file     string
		muteFile string
		Cards    map[string]*CardConfig
	}
	tests := []struct {
		name        string
		fields      fields
		wantErr     bool
		fileContent string
	}{
		{
			name: "ConfigKeeper_Save",
			fields: fields{
				file:     "./testdata/ConfigKeeper_Save",
				muteFile: "./testdata/ConfigKeeperMute_Save",
				Cards: map[string]*CardConfig{
					"one": {
						Name:  "xxx",
						Ports: map[string]*PortConfig{},
					},
				},
			},
			wantErr: false,
			fileContent: `{
  "one": {
    "Name": "xxx",
    "Ports": {},
  }
}`,
		},
		{
			name: "ConfigKeeper_Save empty",
			fields: fields{
				file:     "./testdata/ConfigKeeper_Save",
				muteFile: "./testdata/ConfigKeeperMute_Save",
				Cards:    map[string]*CardConfig{},
			},
			wantErr:     false,
			fileContent: "{}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ck := &ConfigKeeper{
				file:     tt.fields.file,
				muteFile: tt.fields.muteFile,
				Cards:    tt.fields.Cards,
			}
			err := ck.Save()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			s, err := os.Stat(tt.fields.file)
			require.NoError(t, err)
			assert.Equal(t, 0644, int(s.Mode())&0777)

			content, err := os.ReadFile(tt.fields.file)
			require.NoError(t, err)
			assert.Equal(t, tt.fileContent, string(content))

			os.Remove(tt.fields.file)
		})
	}
}

func TestConfigKeeper_SetMode(t *testing.T) {
	// 创建临时测试文件
	tmpFile := "./testdata/test_mode_config.json"
	tmpMuteFile := "./testdata/test_mode_mute_config.json"
	defer os.Remove(tmpFile)
	defer os.Remove(tmpMuteFile)

	ck := NewConfigKeeper(tmpFile, tmpMuteFile)

	// 测试用例1: 设置新端口的 Mode
	t.Run("set mode for new port", func(t *testing.T) {
		cardName := "test_card"
		portName := "test_port"
		mode := "a2dp"

		ck.SetMode(cardName, portName, mode)

		// 验证设置成功
		card, port := ck.GetCardAndPortConfig(cardName, portName)
		assert.NotNil(t, card)
		assert.NotNil(t, port)
		assert.Equal(t, mode, port.Mode)
	})

	// 测试用例2: 更新已存在端口的 Mode
	t.Run("update mode for existing port", func(t *testing.T) {
		cardName := "test_card"
		portName := "test_port"
		newMode := "hfp"

		ck.SetMode(cardName, portName, newMode)

		// 验证更新成功
		card, port := ck.GetCardAndPortConfig(cardName, portName)
		assert.NotNil(t, card)
		assert.NotNil(t, port)
		assert.Equal(t, newMode, port.Mode)
	})

	// 测试用例3: 设置空 Mode
	t.Run("set empty mode", func(t *testing.T) {
		cardName := "test_card2"
		portName := "test_port2"
		mode := ""

		ck.SetMode(cardName, portName, mode)

		// 验证设置成功
		card, port := ck.GetCardAndPortConfig(cardName, portName)
		assert.NotNil(t, card)
		assert.NotNil(t, port)
		assert.Equal(t, mode, port.Mode)
	})
}

func TestConfigKeeper_GetMode(t *testing.T) {
	// 创建临时测试文件
	tmpFile := "./testdata/test_get_mode_config.json"
	tmpMuteFile := "./testdata/test_get_mode_mute_config.json"
	defer os.Remove(tmpFile)
	defer os.Remove(tmpMuteFile)

	ck := NewConfigKeeper(tmpFile, tmpMuteFile)

	// 测试用例1: 获取已设置的 Mode
	t.Run("get existing mode", func(t *testing.T) {
		cardName := "test_card"
		portName := "test_port"
		expectedMode := "a2dp_sink"

		ck.SetMode(cardName, portName, expectedMode)
		actualMode := ck.GetMode(cardName, portName)

		assert.Equal(t, expectedMode, actualMode)
	})

	// 测试用例2: 获取未设置的 Mode（应该返回空字符串）
	t.Run("get mode for new port", func(t *testing.T) {
		cardName := "new_card"
		portName := "new_port"

		mode := ck.GetMode(cardName, portName)

		// 新端口的 Mode 应该是空字符串
		assert.Equal(t, "", mode)
	})

	// 测试用例3: 多次设置和获取
	t.Run("multiple set and get", func(t *testing.T) {
		cardName := "multi_card"
		portName := "multi_port"

		modes := []string{"a2dp", "hfp", "hsp", ""}
		for _, expectedMode := range modes {
			ck.SetMode(cardName, portName, expectedMode)
			actualMode := ck.GetMode(cardName, portName)
			assert.Equal(t, expectedMode, actualMode)
		}
	})
}

func TestConfigKeeper_ModePersistence(t *testing.T) {
	// 创建临时测试文件
	tmpFile := "./testdata/test_mode_persistence_config.json"
	tmpMuteFile := "./testdata/test_mode_persistence_mute_config.json"
	defer os.Remove(tmpFile)
	defer os.Remove(tmpMuteFile)

	// 测试 Mode 的持久化
	t.Run("mode persistence", func(t *testing.T) {
		cardName := "persist_card"
		portName := "persist_port"
		mode := "a2dp_sink"

		// 创建第一个实例并设置 Mode
		ck1 := NewConfigKeeper(tmpFile, tmpMuteFile)
		ck1.SetMode(cardName, portName, mode)

		// 创建第二个实例并加载配置
		ck2 := NewConfigKeeper(tmpFile, tmpMuteFile)
		err := ck2.Load()
		require.NoError(t, err)

		// 验证 Mode 被正确持久化和加载
		actualMode := ck2.GetMode(cardName, portName)
		assert.Equal(t, mode, actualMode)
	})
}
