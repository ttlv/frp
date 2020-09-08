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

package client

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/frp/assets"
	"github.com/fatedier/frp/models/auth"
	"github.com/fatedier/frp/models/config"
	"github.com/fatedier/frp/models/msg"
	"github.com/fatedier/frp/utils/log"
	frpNet "github.com/fatedier/frp/utils/net"
	"github.com/fatedier/frp/utils/version"
	"github.com/fatedier/frp/utils/xlog"

	fmux "github.com/hashicorp/yamux"
	"github.com/shopspring/decimal"
)

// Service is a client service.
type Service struct {
	// uniq id got from frps, attach it in loginMsg
	runId string

	// manager control connection with server
	ctl   *Control
	ctlMu sync.RWMutex

	// Sets authentication based on selected method
	authSetter auth.Setter

	cfg         config.ClientCommonConf
	pxyCfgs     map[string]config.ProxyConf
	visitorCfgs map[string]config.VisitorConf
	cfgMu       sync.RWMutex

	// The configuration file used to initialize this client, or an empty
	// string if no configuration file was used.
	cfgFile string

	// This is configured by the login response from frps
	serverUDPPort int

	exit uint32 // 0 means not exit

	// service context
	ctx context.Context
	// call cancel to stop service
	cancel context.CancelFunc
}

func NewService(cfg config.ClientCommonConf, pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf, cfgFile string) (svr *Service, err error) {

	ctx, cancel := context.WithCancel(context.Background())
	svr = &Service{
		authSetter:  auth.NewAuthSetter(cfg.AuthClientConfig),
		cfg:         cfg,
		cfgFile:     cfgFile,
		pxyCfgs:     pxyCfgs,
		visitorCfgs: visitorCfgs,
		exit:        0,
		ctx:         xlog.NewContext(ctx, xlog.New()),
		cancel:      cancel,
	}
	return
}

func (svr *Service) GetController() *Control {
	svr.ctlMu.RLock()
	defer svr.ctlMu.RUnlock()
	return svr.ctl
}

func (svr *Service) Run() error {
	xl := xlog.FromContextSafe(svr.ctx)

	// login to frps
	for {
		conn, session, err := svr.login()
		if err != nil {
			xl.Warn("login to server failed: %v", err)

			// if login_fail_exit is true, just exit this program
			// otherwise sleep a while and try again to connect to server
			if svr.cfg.LoginFailExit {
				return err
			} else {
				time.Sleep(10 * time.Second)
			}
		} else {
			// login success
			ctl := NewControl(svr.ctx, svr.runId, conn, session, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}

	go svr.keepControllerWorking()

	if svr.cfg.AdminPort != 0 {
		// Init admin server assets
		err := assets.Load(svr.cfg.AssetsDir)
		if err != nil {
			return fmt.Errorf("Load assets error: %v", err)
		}

		err = svr.RunAdminServer(svr.cfg.AdminAddr, svr.cfg.AdminPort)
		if err != nil {
			log.Warn("run admin server error: %v", err)
		}
		log.Info("admin server listen on %s:%d", svr.cfg.AdminAddr, svr.cfg.AdminPort)
	}
	<-svr.ctx.Done()
	return nil
}

func (svr *Service) keepControllerWorking() {
	xl := xlog.FromContextSafe(svr.ctx)
	maxDelayTime := 20 * time.Second
	delayTime := time.Second

	// if frpc reconnect frps, we need to limit retry times in 1min
	// current retry logic is sleep 0s, 0s, 0s, 1s, 2s, 4s, 8s, ...
	// when exceed 1min, we will reset delay and counts
	cutoffTime := time.Now().Add(time.Minute)
	reconnectDelay := time.Second
	reconnectCounts := 1

	for {
		<-svr.ctl.ClosedDoneCh()
		if atomic.LoadUint32(&svr.exit) != 0 {
			return
		}

		// the first three retry with no delay
		if reconnectCounts > 3 {
			time.Sleep(reconnectDelay)
			reconnectDelay *= 2
		}
		reconnectCounts++

		now := time.Now()
		if now.After(cutoffTime) {
			// reset
			cutoffTime = now.Add(time.Minute)
			reconnectDelay = time.Second
			reconnectCounts = 1
		}

		for {
			xl.Info("try to reconnect to server...")
			conn, session, err := svr.login()
			if err != nil {
				xl.Warn("reconnect to server error: %v", err)
				time.Sleep(delayTime)
				delayTime = delayTime * 2
				if delayTime > maxDelayTime {
					delayTime = maxDelayTime
				}
				continue
			}
			// reconnect success, init delayTime
			delayTime = time.Second

			ctl := NewControl(svr.ctx, svr.runId, conn, session, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			if svr.ctl != nil {
				svr.ctl.Close()
			}
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}
}

// login creates a connection to frps and registers it self as a client
// conn: control connection
// session: if it's not nil, using tcp mux
func (svr *Service) login() (conn net.Conn, session *fmux.Session, err error) {
	xl := xlog.FromContextSafe(svr.ctx)
	var tlsConfig *tls.Config
	if svr.cfg.TLSEnable {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	conn, err = frpNet.ConnectServerByProxyWithTLS(svr.cfg.HttpProxy, svr.cfg.Protocol,
		fmt.Sprintf("%s:%d", svr.cfg.ServerAddr, svr.cfg.ServerPort), tlsConfig)
	if err != nil {
		return
	}

	defer func() {
		if err != nil {
			conn.Close()
			if session != nil {
				session.Close()
			}
		}
	}()

	if svr.cfg.TcpMux {
		fmuxCfg := fmux.DefaultConfig()
		fmuxCfg.KeepAliveInterval = 20 * time.Second
		fmuxCfg.LogOutput = ioutil.Discard
		session, err = fmux.Client(conn, fmuxCfg)
		if err != nil {
			return
		}
		stream, errRet := session.OpenStream()
		if errRet != nil {
			session.Close()
			err = errRet
			return
		}
		conn = stream
	}
	loginMsg := &msg.Login{
		Arch:      runtime.GOARCH,
		Os:        runtime.GOOS,
		PoolCount: svr.cfg.PoolCount,
		User:      svr.cfg.User,
		Version:   version.Full(),
		Timestamp: time.Now().Unix(),
		RunId:     svr.runId,
		Metas:     svr.cfg.Metas,
		UniqueID:  getHashResult(),
	}

	// Add auth
	if err = svr.authSetter.SetLogin(loginMsg); err != nil {
		return
	}

	if err = msg.WriteMsg(conn, loginMsg); err != nil {
		return
	}

	var loginRespMsg msg.LoginResp
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = msg.ReadMsgInto(conn, &loginRespMsg); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	if loginRespMsg.Error != "" {
		err = fmt.Errorf("%s", loginRespMsg.Error)
		xl.Error("%s", loginRespMsg.Error)
		return
	}

	svr.runId = loginRespMsg.RunId
	xl.ResetPrefixes()
	xl.AppendPrefix(svr.runId)

	svr.serverUDPPort = loginRespMsg.ServerUdpPort
	xl.Info("login to server success, get run id [%s], server udp port [%d]", loginRespMsg.RunId, loginRespMsg.ServerUdpPort)
	return
}

func (svr *Service) ReloadConf(pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf) error {
	svr.cfgMu.Lock()
	svr.pxyCfgs = pxyCfgs
	svr.visitorCfgs = visitorCfgs
	svr.cfgMu.Unlock()

	return svr.ctl.ReloadConf(pxyCfgs, visitorCfgs)
}

func (svr *Service) Close() {
	atomic.StoreUint32(&svr.exit, 1)
	svr.ctl.Close()
	svr.cancel()
}

func getHashResult() string {
	var deciamlArraies []decimal.Decimal
	interfaces, _ := net.Interfaces()
	for _, inter := range interfaces {
		if fmt.Sprintf("%v", inter.HardwareAddr) == "" {
			continue
		}
		hex := strings.Replace(fmt.Sprintf("%v", inter.HardwareAddr), ":", "", -1)
		deciamlArraies = append(deciamlArraies, decimal.NewFromBigInt(hexToBigInt(hex), 1))
	}
	hashInstance := sha1.New()
	hashInstance.Write([]byte(sortDecimalArray(deciamlArraies)))
	bytes := hashInstance.Sum(nil)
	return fmt.Sprintf("%x", bytes)[20:]
}

func hexToBigInt(hex string) *big.Int {
	n := new(big.Int)
	n, _ = n.SetString(hex[2:], 16)

	return n
}

func sortDecimalArray(deciamlArraies []decimal.Decimal) string {
	min := deciamlArraies[0]
	for _, item := range deciamlArraies {
		if item.LessThan(min) {
			min = item
		}
	}
	return min.String()
}
