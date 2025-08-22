// SPDX-FileCopyrightText: 2018 - 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later

package uadp

import (
	"github.com/linuxdeepin/dde-daemon/loader"
	"github.com/linuxdeepin/go-lib/dbusutil"
	"github.com/linuxdeepin/go-lib/log"
)

const (
	dbusServiceName = "org.deepin.dde.Uadp1"
	dbusPath        = "/org/deepin/dde/Uadp1"
	dbusInterface   = dbusServiceName
)

const (
	dbusServiceName2 = "com.deepin.daemon.Uadp"
	dbusPath2        = "/com/deepin/daemon/Uadp"
	dbusInterface2   = dbusServiceName2
)

var logger = log.NewLogger("daemon/system/uadp")

func init() {
	loader.Register(NewDaemon(logger))
}

type Daemon struct {
	*loader.ModuleBase
	manager *Manager
}

func NewDaemon(logger *log.Logger) *Daemon {
	daemon := new(Daemon)
	daemon.ModuleBase = loader.NewModuleBase("uadp", daemon, logger)
	return daemon
}

func (d *Daemon) GetDependencies() []string {
	return []string{}
}

func (d *Daemon) Start() (err error) {
	logger.Debug("start")
	service := loader.GetService()
	d.manager = newManager(service)

	err = service.Export(dbusPath, d.manager)
	if err != nil {
		return
	}

	err = service.RequestName(dbusServiceName)
	if err != nil {
		logger.Warning(err)
	}

	service2, _ := dbusutil.NewSystemService()
	if service2 == nil {
		logger.Warning("serivce2 is nil")
	} else {
		err = service2.ExportExt(dbusPath2, dbusInterface2, d.manager)
		if err != nil {
			logger.Warning(err)
		}

		err = service2.RequestName(dbusServiceName2)
		if err != nil {
			logger.Warning(err)
		}
		go service2.Wait()
	}

	d.manager.start()
	return
}

func (d *Daemon) Stop() error {
	if d.manager == nil {
		return nil
	}
	d.manager.stop()
	d.manager = nil
	return nil
}
