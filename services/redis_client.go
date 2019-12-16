package services

import (
	"fmt"
	"sync"

	"github.com/go-redis/redis"

	"github.com/urfave/cli"
)

type RedisClient struct {
	host   string
	port   int
	value  *redis.Client
	inited bool
	mux    sync.Mutex
}

const (
	REDIS_HOST_FLAG = "redis-host"
	REDIS_PORT_FLAG = "redis-port"
)

func NewRedisClient(c *cli.Context) *RedisClient {
	return &RedisClient{host: c.String(REDIS_HOST_FLAG), port: c.Int(REDIS_PORT_FLAG), inited: false}
}

func (s *RedisClient) Close() {
	if s.value != nil {
		s.value.Close()
	}
}

func (s *RedisClient) get() *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%d", s.host, s.port),
		Password: "",
		DB:       0,
	})
	return client
}

func (s *RedisClient) Get() *redis.Client {
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
}
