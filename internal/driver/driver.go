/*******************************************************************************
 * Copyright 2017.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License
 * is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
 * or implied. See the License for the specific language governing permissions and limitations under
 * the License.
 *******************************************************************************/

// Package driver HTTP 协议驱动：设备用 HTTP POST 上报数据，用轮询接口取平台下发的命令。
package driver

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/winc-link/hummingbird-sdk-go/commons"
	"github.com/winc-link/hummingbird-sdk-go/model"
	"github.com/winc-link/hummingbird-sdk-go/service"

	drivercfg "github.com/iotserver-org/iotserver-http-driver/config"
	"github.com/iotserver-org/iotserver-http-driver/internal/device"
	"github.com/iotserver-org/iotserver-http-driver/internal/server"
)

// pendingCommand 一条待设备拉取的下行命令。
type pendingCommand struct {
	Cmd  string      `json:"cmd"`
	Data interface{} `json:"data"`
}

// HttpProtocolDriver HTTP 协议驱动。
type HttpProtocolDriver struct {
	sd *service.DriverService

	mu       sync.Mutex
	commands map[string][]pendingCommand // deviceSn → 待拉取命令队列
}

// CloudPluginNotify 云插件启动/停止通知：本驱动不依赖云插件，仅记录日志。
func (dr *HttpProtocolDriver) CloudPluginNotify(ctx context.Context, t commons.CloudPluginNotifyType, name string) error {
	dr.sd.GetLogger().Infof("cloud plugin notify: type=%s name=%s", t, name)
	return nil
}

// DeviceNotify 设备添加/修改/删除通知：维护驱动侧设备表。
func (dr *HttpProtocolDriver) DeviceNotify(ctx context.Context, t commons.DeviceNotifyType, deviceId string, dev model.Device) error {
	switch t {
	case commons.DeviceAddNotify, commons.DeviceUpdateNotify:
		existing, ok := device.Find(dev.DeviceSn)
		online := false
		if ok {
			online = existing.Online
		}
		device.Put(&device.Device{
			DeviceId:  dev.Id,
			DeviceSn:  dev.DeviceSn,
			ProductId: dev.ProductId,
			Online:    online,
		})
		dr.sd.GetLogger().Infof("device %s: id=%s sn=%s", t, deviceId, dev.DeviceSn)
	case commons.DeviceDeleteNotify:
		if dev, ok := device.FindById(deviceId); ok {
			device.Remove(dev.DeviceSn)
		}
		dr.sd.GetLogger().Infof("device delete: id=%s", deviceId)
	default:
		dr.sd.GetLogger().Warnf("unknown device notify type: %s", t)
	}
	return nil
}

// ProductNotify 产品添加/修改/删除通知：物模型由 SDK 缓存，这里仅记录日志。
func (dr *HttpProtocolDriver) ProductNotify(ctx context.Context, t commons.ProductNotifyType, productId string, product model.Product) error {
	dr.sd.GetLogger().Infof("product notify: type=%s id=%s", t, productId)
	return nil
}

// Stop 驱动退出：全部设备置离线。
func (dr *HttpProtocolDriver) Stop(ctx context.Context) error {
	for _, dev := range device.All() {
		_ = dr.sd.Offline(dev.GetDeviceId())
	}
	dr.sd.GetLogger().Info("http driver stopped")
	return nil
}

// HandlePropertySet 属性设置：入队等待设备轮询拉取（HTTP 无法主动推送）。
func (dr *HttpProtocolDriver) HandlePropertySet(ctx context.Context, deviceId string, data model.PropertySet) error {
	return dr.enqueue(deviceId, "set", data)
}

// HandlePropertyGet 属性查询：入队等待设备轮询拉取。
func (dr *HttpProtocolDriver) HandlePropertyGet(ctx context.Context, deviceId string, data model.PropertyGet) error {
	return dr.enqueue(deviceId, "get", data)
}

// HandleServiceExecute 服务调用：入队等待设备轮询拉取。
func (dr *HttpProtocolDriver) HandleServiceExecute(ctx context.Context, deviceId string, data model.ServiceExecuteRequest) error {
	return dr.enqueue(deviceId, "service", data)
}

// enqueue 把下行命令放进设备队列，设备下次轮询时取走。
func (dr *HttpProtocolDriver) enqueue(deviceId, action string, data interface{}) error {
	dev, ok := device.FindById(deviceId)
	if !ok {
		return fmt.Errorf("device [%s] not found", deviceId)
	}
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if dr.commands == nil {
		dr.commands = make(map[string][]pendingCommand)
	}
	dr.commands[dev.DeviceSn] = append(dr.commands[dev.DeviceSn], pendingCommand{Cmd: action, Data: data})
	dr.sd.GetLogger().Infof("downlink %s queued for device %s (sn=%s)", action, deviceId, dev.DeviceSn)
	return nil
}

// takeCommands 取走并清空某设备的待执行命令（设备轮询时调用）。
func (dr *HttpProtocolDriver) takeCommands(deviceSn string) []map[string]interface{} {
	dr.mu.Lock()
	cmds := dr.commands[deviceSn]
	delete(dr.commands, deviceSn)
	dr.mu.Unlock()

	out := make([]map[string]interface{}, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, map[string]interface{}{"cmd": c.Cmd, "data": c.Data})
	}
	return out
}

// handleFrame 处理一次 HTTP 上报。
//
// kind = "properties"：body 为属性键值对，如 {"temp": 23.5, "rpm": 1450}
// kind = "events"：body 为 {事件码: 输出参数}
func (dr *HttpProtocolDriver) handleFrame(deviceSn, kind string, body map[string]interface{}) (int, interface{}) {
	dev, ok := device.Find(deviceSn)
	if !ok {
		// 平台上还没有这个设备：按配置决定是否自动创建
		if !drivercfg.GetConfig().AutoCreateDevice {
			return http.StatusNotFound, map[string]interface{}{
				"ok":    false,
				"error": fmt.Sprintf("device sn=%s is not registered on platform (set AutoCreateDevice=true to auto create)", deviceSn),
			}
		}
		created, err := dr.sd.CreateDevice(model.NewAddDevice(deviceSn, "", deviceSn, "auto created by http driver", nil))
		if err != nil {
			return http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()}
		}
		dev = &device.Device{DeviceId: created.Id, DeviceSn: deviceSn, ProductId: created.ProductId}
		device.Put(dev)
		dr.sd.GetLogger().Infof("device auto created: sn=%s id=%s", deviceSn, created.Id)
	}

	// 首次收到数据视为上线
	if !dev.Online {
		if err := dr.sd.Online(dev.DeviceId); err != nil {
			dr.sd.GetLogger().Errorf("device online failed id=%s: %v", dev.DeviceId, err)
		}
		dev.Online = true
	}

	if kind == "events" {
		for code, raw := range body {
			params, _ := raw.(map[string]interface{})
			if _, err := dr.sd.EventReport(dev.DeviceId, model.NewEventReport(false, model.NewEventData(code, params))); err != nil {
				return http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()}
			}
		}
		return http.StatusOK, map[string]interface{}{"ok": true, "reported": "events"}
	}

	props := make(map[string]model.PropertyData, len(body))
	for code, value := range body {
		if code == "ts" || code == "sn" {
			continue
		}
		props[code] = model.NewPropertyData(value)
	}
	if len(props) == 0 {
		return http.StatusOK, map[string]interface{}{"ok": true, "reported": 0}
	}
	if _, err := dr.sd.PropertyReport(dev.DeviceId, model.NewPropertyReport(false, props)); err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()}
	}
	dr.sd.GetLogger().Infof("property reported device=%s count=%d", dev.DeviceId, len(props))
	return http.StatusOK, map[string]interface{}{"ok": true, "reported": len(props)}
}

// loadDevices 启动时把平台上已注册的设备装进内存表。
func loadDevices(sd *service.DriverService) {
	for _, dev := range sd.GetDeviceList() {
		device.Put(&device.Device{
			DeviceId:  dev.Id,
			DeviceSn:  dev.DeviceSn,
			ProductId: dev.ProductId,
			Online:    dev.Status == commons.DeviceOnline,
		})
	}
}

// NewHttpProtocolDriver 构造 HTTP 协议驱动：装载设备表 → 启动 HTTP 服务。
func NewHttpProtocolDriver(sd *service.DriverService) *HttpProtocolDriver {
	dr := &HttpProtocolDriver{
		sd:       sd,
		commands: make(map[string][]pendingCommand),
	}
	loadDevices(sd)
	svr := server.NewHttpService(sd, dr.handleFrame, dr.takeCommands)
	go svr.Start()
	return dr
}