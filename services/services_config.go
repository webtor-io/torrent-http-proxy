package services

import (
	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"gopkg.in/yaml.v3"
	"os"
)

const (
	configFlag = "config"
)

type Distribution string

const (
	Hash     Distribution = "Hash"
	NodeHash Distribution = "NodeHash"
)

func RegisterServicesConfigFlags(flags []cli.Flag) []cli.Flag {
	return append(flags, &cli.StringFlag{
		Name:     configFlag,
		Usage:    "Path to the services configuration YAML file",
		EnvVar:   "CONFIG_PATH",
		Required: true,
	})
}

type ServiceConfig struct {
	Name            string            `yaml:"name"`
	Distribution    Distribution      `yaml:"distribution"`
	PreferLocalNode bool              `yaml:"preferLocalNode"`
	Headers         map[string]string `yaml:"headers"`
}

type ServicesConfig map[string]*ServiceConfig

func (s ServicesConfig) GetMods() []string {
	var res []string
	for k := range map[string]*ServiceConfig(s) {
		if k != "default" {
			res = append(res, k)
		}
	}
	return res
}

func (s ServicesConfig) GetMod(name string) *ServiceConfig {
	return map[string]*ServiceConfig(s)[name]
}

func (s ServicesConfig) GetDefault() *ServiceConfig {
	for k, v := range map[string]*ServiceConfig(s) {
		if k == "default" {
			return v
		}
	}
	return nil
}

func LoadServicesConfigFromYAML(c *cli.Context) (*ServicesConfig, error) {
	filename := c.String(configFlag)
	if filename == "" {
		return nil, errors.New("no config file provided")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	s := &ServicesConfig{}
	if err := yaml.Unmarshal(data, s); err != nil {
		return nil, err
	}
	return s, nil
}
