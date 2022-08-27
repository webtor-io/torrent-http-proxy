package services

import "github.com/urfave/cli"

const (
	K8S_LABEL_PREFIX = "webtor.io/"
)

const (
	MY_NODE_NAME = "my-node-name"
)

func RegisterCommonFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MY_NODE_NAME,
		Usage:  "My node name",
		Value:  "",
		EnvVar: "MY_NODE_NAME",
	})
}
