package services

import (
	"io/ioutil"
	"path/filepath"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type Client struct {
	Name   string `yaml:"name"`
	ApiKey string `yaml:"apiKey"`
	Secret string `yaml:"secret"`
}

type Clients []Client

func NewClients() (*Clients, error) {
	var cc Clients
	filename, _ := filepath.Abs("/etc/config/clients.yaml")
	data, err := ioutil.ReadFile(filename)

	if err != nil {
		return &cc, nil
	}

	err = yaml.Unmarshal(data, &cc)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to unmarshal clients config")
	}
	return &cc, nil
}

func (s Clients) Get(apiKey string) *Client {
	for _, c := range s {
		if c.ApiKey == apiKey {
			return &c
		}
	}
	return nil
}
func (s Clients) Empty() bool {
	return len(s) == 0
}
