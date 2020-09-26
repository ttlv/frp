package config

import (
	"github.com/jinzhu/configor"
)

type FrpAdapterConfig struct {
	Address string
}

type FrpsConfig struct {
	HttpAuthUserName string
	HttpAuthPassword string
	Api              string
}

var _frpAdapterConfig *FrpAdapterConfig
var _frpsConfig *FrpsConfig

func MustGetFrpAdapterConfig() FrpAdapterConfig {
	if _frpAdapterConfig != nil {
		return *_frpAdapterConfig
	}

	_frpAdapterConfig = &FrpAdapterConfig{}
	err := configor.New(&configor.Config{ENVPrefix: "FRP_ADAPTER"}).Load(_frpAdapterConfig)
	if err != nil {
		panic(err)
	}

	return *_frpAdapterConfig
}
