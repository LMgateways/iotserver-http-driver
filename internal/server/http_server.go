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

// Package server HTTP 接入服务：设备/网关用 HTTP POST 上报数据。
//
// 接口：
//
//	POST /ingest/{deviceSn}          上报属性，body 为属性键值对 JSON
//	POST /ingest/{deviceSn}/events   上报事件，body 为 {事件码: 输出参数}
//	GET  /health                     健康检查
//
// 鉴权：自定义参数 Token 非空时，请求必须带 `X-Token: <token>` 头。
package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/LMgateways/iotserver-sdk-go/service"

	drivercfg "github.com/LMgateways/iotserver-http-driver/config"
)

var GlobalDriverService *service.DriverService

// FrameHandler 业务处理回调：收到一帧上报后由驱动实现处理（上报平台等）。
type FrameHandler func(deviceSn, kind string, body map[string]interface{}) (int, interface{})

// CommandProvider 下行命令提供者：设备轮询时取走待执行命令（HTTP 无法主动推送）。
type CommandProvider func(deviceSn string) []map[string]interface{}

// HttpServer HTTP 接入服务。
type HttpServer struct {
	sd      *service.DriverService
	handler FrameHandler
	// cmds 下行命令来源；为 nil 时轮询接口返回空列表
	cmds CommandProvider
	srv  *http.Server
}

// NewHttpService 构造（不启动）。cmds 可为 nil。
func NewHttpService(sd *service.DriverService, handler FrameHandler, cmds CommandProvider) *HttpServer {
	return &HttpServer{sd: sd, handler: handler, cmds: cmds}
}

// Start 启动 HTTP 服务（阻塞，放到 goroutine 里跑）。
func (s *HttpServer) Start() {
	GlobalDriverService = s.sd

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ingest/", s.handleIngest)

	addr := drivercfg.GetConfig().ListenAddr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.sd.GetLogger().Errorf("http listen %s failed: %v", addr, err)
		return
	}
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
	s.sd.GetLogger().Infof("http server listening on %s", addr)
	if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		s.sd.GetLogger().Errorf("http server exited: %v", err)
	}
}

// Stop 关闭服务。
func (s *HttpServer) Stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

func (s *HttpServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "time": time.Now().UnixMilli()})
}

// handleIngest 处理：
//
//	POST /ingest/{deviceSn}            上报属性
//	POST /ingest/{deviceSn}/events     上报事件
//	GET  /ingest/{deviceSn}/commands   轮询取平台下发的命令
func (s *HttpServer) handleIngest(w http.ResponseWriter, r *http.Request) {
	// 鉴权
	if token := drivercfg.GetConfig().Token; token != "" {
		if r.Header.Get("X-Token") != token {
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"ok": false, "error": "invalid token"})
			return
		}
	}

	// 解析路径：/ingest/{deviceSn} 或 /ingest/{deviceSn}/{events|commands}
	rest := strings.TrimPrefix(r.URL.Path, "/ingest/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "missing deviceSn in path"})
		return
	}
	deviceSn := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	// 命令轮询（设备侧主动来取）
	if sub == "commands" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "GET only"})
			return
		}
		cmds := []map[string]interface{}{}
		if s.cmds != nil {
			cmds = s.cmds(deviceSn)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "commands": cmds})
		return
	}

	// 数据上报
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	kind := "properties"
	if sub == "events" {
		kind = "events"
	}

	// 读 body（限制 1MB，防止超大请求打爆内存）
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	var frame map[string]interface{}
	if err := json.Unmarshal(body, &frame); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "invalid json"})
		return
	}

	status, resp := s.handler(deviceSn, kind, frame)
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}