package server_test

import "github.com/bitsalt/bitblocker/internal/config"

func configListen(host string, port int) config.ListenConfig {
	return config.ListenConfig{Host: host, Port: port}
}
