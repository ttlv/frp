// Copyright 2018 fatedier, fatedier@gmail.com
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

package main

import (
	"fmt"
	"github.com/ttlv/common_utils/config/frp_adapter"
	"github.com/ttlv/common_utils/utils"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fatedier/golib/crypto"

	_ "github.com/fatedier/frp/assets/frps/statik"
	_ "github.com/fatedier/frp/models/metrics"
)

func main() {
	crypto.DefaultSalt = "frp"
	rand.Seed(time.Now().UnixNano())
	signalChan := make(chan os.Signal, 1)
	go func() {
		<-signalChan
		r, err := utils.Put(fmt.Sprintf("%v/nm_useless", frp_adapter.MustGetFrpAdapterConfig().Address), nil, nil, nil)
		if err != nil {
			log.Println(err)
		}
		log.Println(r)
		os.Exit(1)
	}()
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	Execute()

}
