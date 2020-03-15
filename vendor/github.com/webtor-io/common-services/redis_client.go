package services

import (
	"fmt"
	"sync"

	"github.com/go-redis/redis"

	"github.com/urfave/cli"

	log "github.com/sirupsen/logrus"
)

type RedisClient struct {
	host               string
	port               int
	sentinelPort       int
	sentinelMasterName string
	value              redis.UniversalClient
	inited             bool
	mux                sync.Mutex
}

const (
	REDIS_HOST_FLAG            = "redis-host"
	REDIS_PORT_FLAG            = "redis-port"
	REDIS_SENTINEL_PORT_FLAG   = "redis-sentinel-port"
	REDIS_SENTINEL_MASTER_NAME = "redis-sentinel-master-name"
)

func NewRedisClient(c *cli.Context) *RedisClient {
	return &RedisClient{host: c.String(REDIS_HOST_FLAG), port: c.Int(REDIS_PORT_FLAG),
		sentinelPort: c.Int(REDIS_SENTINEL_PORT_FLAG), sentinelMasterName: c.String(REDIS_SENTINEL_MASTER_NAME),
		inited: false}
}

func (s *RedisClient) Close() {
	if s.value != nil {
		s.value.Close()
	}
}

func (s *RedisClient) get() redis.UniversalClient {
	if s.sentinelPort != 0 {
		addrs := []string{fmt.Sprintf("%s:%d", s.host, s.sentinelPort)}
		log.Infof("Using sentinel redis client with addrs=%v and master name=%v", addrs, s.sentinelMasterName)
		return redis.NewUniversalClient(&redis.UniversalOptions{
			Addrs:      addrs,
			Password:   "",
			DB:         0,
			MasterName: s.sentinelMasterName,
		})
	}
	addrs := []string{fmt.Sprintf("%s:%d", s.host, s.port)}
	log.Infof("Using default redis client with addrs=%v", addrs)
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    addrs,
		Password: "",
		DB:       0,
	})
}

func (s *RedisClient) Get() redis.UniversalClient {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.value
	}
	s.value = s.get()
	s.inited = true
	return s.value
}

func RegisterRedisClientFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   REDIS_HOST_FLAG,
		Usage:  "redis host",
		Value:  "localhost",
		EnvVar: "REDIS_MASTER_SERVICE_HOST, REDIS_SERVICE_HOST",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   REDIS_PORT_FLAG,
		Usage:  "redis port",
		Value:  6379,
		EnvVar: "REDIS_MASTER_SERVICE_PORT, REDIS_SERVICE_PORT",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   REDIS_SENTINEL_PORT_FLAG,
		Usage:  "redis sentinel port",
		EnvVar: "REDIS_SERVICE_PORT_REDIS_SENTINEL",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   REDIS_SENTINEL_MASTER_NAME,
		Usage:  "redis sentinel master name",
		Value:  "mymaster",
		EnvVar: "REDIS_SERVICE_SENTINEL_MASTER_NAME",
	})
}
