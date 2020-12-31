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
	s.RegisterGRPCFlags(app)
	cs.RegisterRedisClientFlags(app)
	s.RegisterJobFlags(app)
	s.RegisterConnectionConfigFlags(app)
	cs.RegisterProbeFlags(app)
	s.RegisterNodesStatFlags(app)
	s.RegisterPromFlags(app)
	s.RegisterSubdomainsFlags(app)

	app.Action = run
}

func run(c *cli.Context) error {

	// Setting Clients
	clients, err := s.NewClients()

	if err != nil {
		log.WithError(err).Error("Got clients error")
		return err
	}

	// Setting Base URL
	baseURL := s.GetBaseURL()

	// Setting Config
	config := s.NewConnectionsConfig(c)

	// Setting URL Parser
	urlParser := s.NewURLParser(config)

	// Setting Kubernetes client
	k8sClient := s.NewK8SClient()

	// Setting Prometheus client
	promClient := s.NewPromClient(c)

	// Setting Redis client
	redisClient := cs.NewRedisClient(c)
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
	httpProxyPool := s.NewHTTPProxyPool(resolver)

	// Setting Claims
	claims := s.NewClaims(clients)

	if err != nil {
		log.WithError(err).Error("Got claim error")
		return err
	}

	// Setting GRPC Proxy Pool
	grpcProxyPool := s.NewHTTPGRPCProxyPool(baseURL, claims, resolver)

	// Setting NodesStat Pool
	nodesStatPool := s.NewNodesStatPool(c, promClient, k8sClient, log.NewEntry(log.StandardLogger()))

	// Setting Subdomains Pool
	subdomainsPool := s.NewSubdomainsPool(c, k8sClient, nodesStatPool)

	// Setting WebService
	web := s.NewWeb(c, baseURL, urlParser, resolver, httpProxyPool, grpcProxyPool, claims, subdomainsPool)
	defer web.Close()

	// Setting GRPC Proxy
	grpcProxy := s.NewGRPCProxy(baseURL, claims, resolver, nil, urlParser, log.WithFields(log.Fields{}))

	// Setting GRPC Server
	grpcServer := s.NewGRPCServer(c, grpcProxy)
	defer grpcServer.Close()

	// Setting ServeService
	serve := cs.NewServe(probe, web, grpcServer)

	// And SERVE!
	err = serve.Serve()
	if err != nil {
		log.WithError(err).Error("Got serve error")
	}
	return err
}
