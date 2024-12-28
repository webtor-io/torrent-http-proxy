package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/torrent-http-proxy/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}
	app.Flags = cs.RegisterProbeFlags(app.Flags)
	app.Flags = cs.RegisterPromFlags(app.Flags)
	app.Flags = cs.RegisterPprofFlags(app.Flags)
	app.Flags = s.RegisterWebFlags(app.Flags)
	app.Flags = s.RegisterClickHouseFlags(app.Flags)
	app.Flags = s.RegisterClickHouseDBFlags(app.Flags)
	app.Flags = s.RegisterCommonFlags(app.Flags)
	app.Flags = s.RegisterEndpointsFlags(app.Flags)
	app.Flags = s.RegisterHTTPProxyFlags(app.Flags)
	app.Flags = s.RegisterAPIFlags(app.Flags)
	app.Flags = s.RegisterServicesConfigFlags(app.Flags)

	app.Action = run
}

func run(c *cli.Context) error {
	var servers []cs.Servable

	// Setting Config
	config, err := s.LoadServicesConfigFromYAML(c)

	if err != nil {
		return err
	}

	// Setting URL Parser
	urlParser := s.NewURLParser(config)

	// Setting Bucket
	bucket := s.NewBucket()

	// Setting Kubernetes client
	k8sClient := s.NewK8SClient()

	// Setting K8SEndpoints
	endpointsPool := s.NewEndpoints(c, k8sClient)

	// Setting ServiceLocation
	svcLocPool := s.NewServiceLocationPool(c, endpointsPool)

	// Setting Resolver
	resolver := s.NewResolver(config, svcLocPool)

	// Setting Probe
	probe := cs.NewProbe(c)
	if probe != nil {
		servers = append(servers, probe)
		defer probe.Close()
	}

	// Setting Prom
	prom := cs.NewProm(c)
	if prom != nil {
		servers = append(servers, prom)
	}

	// Setting Pprof
	pprof := cs.NewPprof(c)
	if pprof != nil {
		servers = append(servers, pprof)
		defer prom.Close()
	}

	// Setting HTTP Proxy Pool
	httpProxy := s.NewHTTPProxy(c, resolver)

	// Setting Claims
	claims := s.NewClaims(c)

	var clickHouse *s.ClickHouse

	if c.String(s.ClickhouseDSNFlag) != "" {
		// Setting ClickHouse DB
		clickHouseDB := s.NewClickHouseDB(c)
		defer clickHouseDB.Close()

		// Setting ClickHouse
		clickHouse = s.NewClickHouse(c, clickHouseDB)
		if clickHouse != nil {
			defer clickHouse.Close()
		}
	}

	// Setting AccessHistory
	accessHistory := s.NewAccessHistory()

	// Setting WebService
	web := s.NewWeb(c, urlParser, resolver, httpProxy, claims,
		bucket, clickHouse, config, accessHistory)
	servers = append(servers, web)
	defer web.Close()

	// Setting ServeService
	serve := cs.NewServe(servers...)

	// And SERVE!
	err = serve.Serve()
	if err != nil {
		log.WithError(err).Error("got serve error")
	}
	return err
}
