package services

import "github.com/urfave/cli"

const (
	myNodeNameFlag = "my-node-name"
)

func RegisterCommonFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   myNodeNameFlag,
			Usage:  "My node name",
			Value:  "",
			EnvVar: "MY_NODE_NAME",
		},
	)
}
