// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	ttlv_utils "github.com/ttlv/common_utils/utils"
	"io"
	"net"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/frp/models/auth"
	"github.com/fatedier/frp/models/config"
	"github.com/fatedier/frp/models/consts"
	frpErr "github.com/fatedier/frp/models/errors"
	"github.com/fatedier/frp/models/msg"
	plugin "github.com/fatedier/frp/models/plugin/server"
	"github.com/fatedier/frp/server/controller"
	"github.com/fatedier/frp/server/metrics"
	"github.com/fatedier/frp/server/proxy"
	"github.com/fatedier/frp/utils/util"
	"github.com/fatedier/frp/utils/version"
	"github.com/fatedier/frp/utils/xlog"

	"github.com/fatedier/golib/control/shutdown"
	"github.com/fatedier/golib/crypto"
	"github.com/fatedier/golib/errors"
	"github.com/tidwall/gjson"
	"github.com/ttlv/frp_adapter/app/entries"
)

type ControlManager struct {
	// controls indexed by run id
	ctlsByRunId map[string]*Control

	mu sync.RWMutex
}

func NewControlManager() *ControlManager {
	return &ControlManager{
		ctlsByRunId: make(map[string]*Control),
	}
}

func (cm *ControlManager) Add(runId string, ctl *Control) (oldCtl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	oldCtl, ok := cm.ctlsByRunId[runId]
	if ok {
		oldCtl.Replaced(ctl)
	}
	cm.ctlsByRunId[runId] = ctl
	return
}

// we should make sure if it's the same control to prevent delete a new one
func (cm *ControlManager) Del(runId string, ctl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if c, ok := cm.ctlsByRunId[runId]; ok && c == ctl {
		delete(cm.ctlsByRunId, runId)
	}
}

func (cm *ControlManager) GetById(runId string) (ctl *Control, ok bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ctl, ok = cm.ctlsByRunId[runId]
	return
}

type Control struct {
	// all resource managers and controllers
	rc *controller.ResourceController

	// proxy manager
	pxyManager *proxy.ProxyManager

	// plugin manager
	pluginManager *plugin.Manager

	// verifies authentication based on selected method
	authVerifier auth.Verifier

	// login message
	loginMsg *msg.Login

	// control connection
	conn net.Conn

	// put a message in this channel to send it over control connection to client
	sendCh chan (msg.Message)

	// read from this channel to get the next message sent by client
	readCh chan (msg.Message)

	// work connections
	workConnCh chan net.Conn

	// proxies in one client
	proxies map[string]proxy.Proxy

	// pool count
	poolCount int

	// ports used, for limitations
	portsUsedNum int

	// last time got the Ping message
	lastPing time.Time

	// A new run id will be generated when a new client login.
	// If run id got from login message has same run id, it means it's the same client, so we can
	// replace old controller instantly.
	runId string

	// control status
	status string

	readerShutdown  *shutdown.Shutdown
	writerShutdown  *shutdown.Shutdown
	managerShutdown *shutdown.Shutdown
	allShutdown     *shutdown.Shutdown

	mu sync.RWMutex

	// Server configuration information
	serverCfg config.ServerCommonConf

	xl  *xlog.Logger
	ctx context.Context
}

func NewControl(
	ctx context.Context,
	rc *controller.ResourceController,
	pxyManager *proxy.ProxyManager,
	pluginManager *plugin.Manager,
	authVerifier auth.Verifier,
	ctlConn net.Conn,
	loginMsg *msg.Login,
	serverCfg config.ServerCommonConf,
) *Control {

	poolCount := loginMsg.PoolCount
	if poolCount > int(serverCfg.MaxPoolCount) {
		poolCount = int(serverCfg.MaxPoolCount)
	}
	return &Control{
		rc:              rc,
		pxyManager:      pxyManager,
		pluginManager:   pluginManager,
		authVerifier:    authVerifier,
		conn:            ctlConn,
		loginMsg:        loginMsg,
		sendCh:          make(chan msg.Message, 10),
		readCh:          make(chan msg.Message, 10),
		workConnCh:      make(chan net.Conn, poolCount+10),
		proxies:         make(map[string]proxy.Proxy),
		poolCount:       poolCount,
		portsUsedNum:    0,
		lastPing:        time.Now(),
		runId:           loginMsg.RunId,
		status:          consts.Working,
		readerShutdown:  shutdown.New(),
		writerShutdown:  shutdown.New(),
		managerShutdown: shutdown.New(),
		allShutdown:     shutdown.New(),
		serverCfg:       serverCfg,
		xl:              xlog.FromContextSafe(ctx),
		ctx:             ctx,
	}
}

// Start send a login success message to client and start working.
func (ctl *Control) Start() {
	loginRespMsg := &msg.LoginResp{
		Version:       version.Full(),
		RunId:         ctl.runId,
		ServerUdpPort: ctl.serverCfg.BindUdpPort,
		Error:         "",
	}
	msg.WriteMsg(ctl.conn, loginRespMsg)

	go ctl.writer()
	for i := 0; i < ctl.poolCount; i++ {
		ctl.sendCh <- &msg.ReqWorkConn{}
	}

	go ctl.manager()
	go ctl.reader()
	go ctl.stoper()
}

func (ctl *Control) RegisterWorkConn(conn net.Conn) error {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	select {
	case ctl.workConnCh <- conn:
		xl.Debug("new work connection registered")
		return nil
	default:
		xl.Debug("work connection pool is full, discarding")
		return fmt.Errorf("work connection pool is full, discarding")
	}
}

// When frps get one user connection, we get one work connection from the pool and return it.
// If no workConn available in the pool, send message to frpc to get one or more
// and wait until it is available.
// return an error if wait timeout
func (ctl *Control) GetWorkConn() (workConn net.Conn, err error) {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	var ok bool
	// get a work connection from the pool
	select {
	case workConn, ok = <-ctl.workConnCh:
		if !ok {
			err = frpErr.ErrCtlClosed
			return
		}
		xl.Debug("get work connection from pool")
	default:
		// no work connections available in the poll, send message to frpc to get more
		err = errors.PanicToError(func() {
			ctl.sendCh <- &msg.ReqWorkConn{}
		})
		if err != nil {
			xl.Error("%v", err)
			return
		}

		select {
		case workConn, ok = <-ctl.workConnCh:
			if !ok {
				err = frpErr.ErrCtlClosed
				xl.Warn("no work connections avaiable, %v", err)
				return
			}

		case <-time.After(time.Duration(ctl.serverCfg.UserConnTimeout) * time.Second):
			err = fmt.Errorf("timeout trying to get work connection")
			xl.Warn("%v", err)
			return
		}
	}

	// When we get a work connection from pool, replace it with a new one.
	errors.PanicToError(func() {
		ctl.sendCh <- &msg.ReqWorkConn{}
	})
	return
}

func (ctl *Control) Replaced(newCtl *Control) {
	xl := ctl.xl
	xl.Info("Replaced by client [%s]", newCtl.runId)
	ctl.runId = ""
	ctl.allShutdown.Start()
}

func (ctl *Control) writer() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.writerShutdown.Done()

	encWriter, err := crypto.NewWriter(ctl.conn, []byte(ctl.serverCfg.Token))
	if err != nil {
		xl.Error("crypto new writer error: %v", err)
		ctl.allShutdown.Start()
		return
	}
	for {
		if m, ok := <-ctl.sendCh; !ok {
			xl.Info("control writer is closing")
			return
		} else {
			if err := msg.WriteMsg(encWriter, m); err != nil {
				xl.Warn("write message to control connection error: %v", err)
				return
			}
		}
	}
}

func (ctl *Control) reader() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.readerShutdown.Done()

	encReader := crypto.NewReader(ctl.conn, []byte(ctl.serverCfg.Token))
	for {
		if m, err := msg.ReadMsg(encReader); err != nil {
			if err == io.EOF {
				xl.Debug("control connection closed")
				return
			} else {
				xl.Warn("read error: %v", err)
				ctl.conn.Close()
				return
			}
		} else {
			ctl.readCh <- m
		}
	}
}

func (ctl *Control) stoper() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	ctl.allShutdown.WaitStart()

	close(ctl.readCh)
	ctl.managerShutdown.WaitDone()

	close(ctl.sendCh)
	ctl.writerShutdown.WaitDone()

	ctl.conn.Close()
	ctl.readerShutdown.WaitDone()

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	close(ctl.workConnCh)
	for workConn := range ctl.workConnCh {
		workConn.Close()
	}

	for _, pxy := range ctl.proxies {
		pxy.Close()
		ctl.pxyManager.Del(pxy.GetName())
		metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseInfo().ProxyType)
	}

	ctl.allShutdown.Done()
	xl.Info("client exit success")
	metrics.Server.CloseClient()

	// frpc断开与frps的连接时需要设置hook,通知frp adapter服务将节点设置为离线状态
	v := url.Values{}
	v.Add("status", consts.Offline)
	v.Add("unique_id", ctl.loginMsg.UniqueID)
	result, err := ttlv_utils.Put(ctl.serverCfg.FrpAdapterServerAddress+"/frp_update", nil, v, nil)
	if err != nil {
		xl.Info("update frpc info into k8s failed,err is %v", err)
	}
	xl.Info(result)
}

// block until Control closed
func (ctl *Control) WaitClosed() {
	ctl.allShutdown.WaitDone()
}

func (ctl *Control) manager() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.managerShutdown.Done()

	heartbeat := time.NewTicker(time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-heartbeat.C:
			if time.Since(ctl.lastPing) > time.Duration(ctl.serverCfg.HeartBeatTimeout)*time.Second {
				xl.Warn("heartbeat timeout")
				return
			}
		case rawMsg, ok := <-ctl.readCh:
			if !ok {
				return
			}

			switch m := rawMsg.(type) {
			case *msg.NewProxy:
				content := &plugin.NewProxyContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunId: ctl.loginMsg.RunId,
					},
					NewProxy: *m,
				}
				var remoteAddr string
				retContent, err := ctl.pluginManager.NewProxy(content)
				if err == nil {
					m = &retContent.NewProxy
					remoteAddr, err = ctl.RegisterProxy(m)
				}

				// register proxy in this control
				resp := &msg.NewProxyResp{
					ProxyName: m.ProxyName,
				}
				if err != nil {
					xl.Warn("new proxy [%s] error: %v", m.ProxyName, err)
					resp.Error = util.GenerateResponseErrorString(fmt.Sprintf("new proxy [%s] error", m.ProxyName), err, ctl.serverCfg.DetailedErrorsToClient)
				} else {
					resp.RemoteAddr = remoteAddr
					xl.Info("new proxy [%s] success", m.ProxyName)
					metrics.Server.NewProxy(m.ProxyName, m.ProxyType, ctl.loginMsg.UniqueID, ctl.loginMsg.MacAddress, util.GetInternalIp())
					// 设置Frps hook,当有新的frpc注册进来，建立tcp连接时，立刻通知frp_adapter服务
					// 已经注册的节点因为frps服务重启，可能会出现重新分配port的情况，所以需要先去k8s中获取旧的数据进行对比
					// 结果以frps的结果为准，如果两者不一样，则进行更新操作
					var (
						createParams = url.Values{}
						updateParams = url.Values{}
						coreFrp      = entries.CoreFrp{}
					)
					getResult, err := ttlv_utils.Get(fmt.Sprintf("%v/frp_fetch/%v", ctl.serverCfg.FrpAdapterServerAddress, fmt.Sprintf("nodemaintenances-%v", ctl.loginMsg.UniqueID)), nil, nil)
					if err != nil {
						xl.Info("fetch %v from k8s failed,err is %v", fmt.Sprintf("node_maintenance_name-%v", ctl.loginMsg.UniqueID), err)
					}
					if gjson.Get(getResult, "error.code").String() == "400" {
						xl.Info(gjson.Get(getResult, "message").String())
					} else if gjson.Get(getResult, "error.code").String() == "404" {
						// 不存在当前的资源对象，需要创建
						// Frps的公网IP地址
						createParams.Add("frp_server_ip_address", util.GetInternalIp())
						// Frps与Frpc连接的Port
						createParams.Add("port", strings.Replace(remoteAddr, ":", "", -1))
						// Frpc uniqueID
						createParams.Add("unique_id", ctl.loginMsg.UniqueID)
						// Frpc MacAddress
						createParams.Add("mac_address", ctl.loginMsg.MacAddress)
						// Frpc 状态(online|offline)
						createParams.Add("status", consts.Online)
						result, err := ttlv_utils.Post(ctl.serverCfg.FrpAdapterServerAddress+"/frp_create", nil, createParams, nil)
						if err != nil {
							xl.Info("register new frpc info into k8s failed,err is %v", err)
						}
						xl.Info(result)
					} else {
						// 当前的对象已经存在，直接执行更新操作
						json.Unmarshal([]byte(getResult), &coreFrp)
						updateParams.Add("frp_server_ip_address", util.GetInternalIp())
						updateParams.Add("port", strings.Replace(remoteAddr, ":", "", -1))
						updateParams.Add("status", consts.Online)
						updateParams.Add("unique_id", fmt.Sprintf("%v", ctl.loginMsg.UniqueID))
						updateParams.Add("mac_address", ctl.loginMsg.MacAddress)
						result, err := ttlv_utils.Put(ctl.serverCfg.FrpAdapterServerAddress+"/frp_update", nil, updateParams, nil)
						if err != nil {
							xl.Info("update frpc info into k8s failed,err is %v", err)
						}
						xl.Info(result)
					}
				}
				ctl.sendCh <- resp
			case *msg.CloseProxy:
				ctl.CloseProxy(m)
				xl.Info("close proxy [%s] success", m.ProxyName)
			case *msg.Ping:
				content := &plugin.PingContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunId: ctl.loginMsg.RunId,
					},
					Ping: *m,
				}
				retContent, err := ctl.pluginManager.Ping(content)
				if err == nil {
					m = &retContent.Ping
					err = ctl.authVerifier.VerifyPing(m)
				}
				if err != nil {
					xl.Warn("received invalid ping: %v", err)
					ctl.sendCh <- &msg.Pong{
						Error: util.GenerateResponseErrorString("invalid ping", err, ctl.serverCfg.DetailedErrorsToClient),
					}
					return
				}
				ctl.lastPing = time.Now()
				xl.Debug("receive heartbeat")
				ctl.sendCh <- &msg.Pong{}
			}
		}
	}
}

func (ctl *Control) RegisterProxy(pxyMsg *msg.NewProxy) (remoteAddr string, err error) {
	var pxyConf config.ProxyConf
	// Load configures from NewProxy message and check.
	pxyConf, err = config.NewProxyConfFromMsg(pxyMsg, ctl.serverCfg)
	if err != nil {
		return
	}

	// User info
	userInfo := plugin.UserInfo{
		User:  ctl.loginMsg.User,
		Metas: ctl.loginMsg.Metas,
		RunId: ctl.runId,
	}

	// NewProxy will return a interface Proxy.
	// In fact it create different proxies by different proxy type, we just call run() here.
	pxy, err := proxy.NewProxy(ctl.ctx, userInfo, ctl.rc, ctl.poolCount, ctl.GetWorkConn, pxyConf, ctl.serverCfg)
	if err != nil {
		return remoteAddr, err
	}

	// Check ports used number in each client
	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.mu.Lock()
		if ctl.portsUsedNum+pxy.GetUsedPortsNum() > int(ctl.serverCfg.MaxPortsPerClient) {
			ctl.mu.Unlock()
			err = fmt.Errorf("exceed the max_ports_per_client")
			return
		}
		ctl.portsUsedNum = ctl.portsUsedNum + pxy.GetUsedPortsNum()
		ctl.mu.Unlock()

		defer func() {
			if err != nil {
				ctl.mu.Lock()
				ctl.portsUsedNum = ctl.portsUsedNum - pxy.GetUsedPortsNum()
				ctl.mu.Unlock()
			}
		}()
	}

	remoteAddr, err = pxy.Run()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			pxy.Close()
		}
	}()

	err = ctl.pxyManager.Add(pxyMsg.ProxyName, pxy)
	if err != nil {
		return
	}

	ctl.mu.Lock()
	ctl.proxies[pxy.GetName()] = pxy
	ctl.mu.Unlock()

	return
}

func (ctl *Control) CloseProxy(closeMsg *msg.CloseProxy) (err error) {
	ctl.mu.Lock()
	pxy, ok := ctl.proxies[closeMsg.ProxyName]
	if !ok {
		ctl.mu.Unlock()
		return
	}

	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.portsUsedNum = ctl.portsUsedNum - pxy.GetUsedPortsNum()
	}
	pxy.Close()
	ctl.pxyManager.Del(pxy.GetName())
	delete(ctl.proxies, closeMsg.ProxyName)
	ctl.mu.Unlock()

	metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseInfo().ProxyType)
	return
}
