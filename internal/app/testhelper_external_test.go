package app_test

import (
	"github.com/hobeone/gonzbd/internal/config"
)

func testConfig(dl, comp, admin string, servers ...config.ServerConfig) *config.Config {
	cfg, err := config.Default()
	if err != nil {
		panic(err)
	}
	cfg.With(func(c *config.Config) {
		c.General.DownloadDir = dl
		c.General.CompleteDir = comp
		c.General.AdminDir = admin
		c.Servers = servers
	})
	return cfg
}
