package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/torrent-http-proxy/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}

	s.RegisterWebFlags(app)
	s.RegisterClaimsFlags(app)
	s.RegisterRedisClientFlags(app)
	s.RegisterJobFlags(app)
	s.RegisterConnectionConfigFlags(app)
	cs.RegisterProbeFlags(app)

	app.Action = run
}

func run(c *cli.Context) error {
	// Setting Base URL
	baseURL := s.GetBaseURL()

	// Setting Config
	config := s.NewConnectionsConfig(c)

	// Setting URL Parser
	urlParser := s.NewURLParser(config)

	// Setting Kubernetes client
	k8sClient := s.NewK8SClient()

	// Setting Redis client
	redisClient := s.NewRedisClient(c)
	defer redisClient.Close()

	// Setting Locker
	locker := s.NewLocker(redisClient)

	// Setting JobLocationPool
	jobLocPool := s.NewJobLocationPool(c, k8sClient, locker)

	// Setting ServiceLocationPool
	svcLocPool := s.NewServiceLocationPool()

	// Setting Resolver
	resolver := s.NewResolver(baseURL, config, svcLocPool, jobLocPool)

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting HTTP Proxy Pool
	httpProxyPool := s.NewHTTPProxyPool()

	// Setting Claims
	claims := s.NewClaims(c)

	// Setting GRPC Proxy Pool
	grpcProxyPool := s.NewGRPCProxyPool(claims)

	// Setting WebService
	web := s.NewWeb(c, baseURL, urlParser, resolver, httpProxyPool, grpcProxyPool, claims)
	defer web.Close()

	// Setting ServeService
	serve := cs.NewServe(probe, web)

	// And SERVE!
	err := serve.Serve()
	if err != nil {
		log.WithError(err).Error("Got serve error")
	}
	return err
}
