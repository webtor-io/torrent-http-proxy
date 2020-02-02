package services

import "github.com/urfave/cli"

type ConnectionType int

const (
	ConnectionType_SERVICE ConnectionType = 0
	ConnectionType_JOB     ConnectionType = 1
	JOB_AFFINITY                          = "cloud.scaleway.com/scw-poolname"
	JOB_GRACE                             = 600
)

type ServiceConfig struct {
	EnvName string
}

type JobConfig struct {
	Name         string
	Image        string
	CPURequests  string
	CPULimits    string
	Grace        int
	IgnoredPaths []string
}

type ConnectionConfig struct {
	ServiceConfig
	JobConfig
	Name           string
	ConnectionType ConnectionType
	Mod            bool
}

type ConnectionsConfig map[string]*ConnectionConfig

func (s ConnectionsConfig) GetMods() []string {
	res := []string{}
	for k, _ := range map[string]*ConnectionConfig(s) {
		if k != "default" {
			res = append(res, k)
		}
	}
	return res
}

func (s ConnectionsConfig) GetMod(name string) *ConnectionConfig {
	return map[string]*ConnectionConfig(s)[name]
}

func (s ConnectionsConfig) GetDefault() *ConnectionConfig {
	for k, v := range map[string]*ConnectionConfig(s) {
		if k == "default" {
			return v
		}
	}
	return nil
}

func (s *JobConfig) CheckIgnorePaths(name string) bool {
	for _, p := range s.IgnoredPaths {
		if p == name {
			return true
		}
	}
	return false
}

const (
	SEEDER_IMAGE            = "seeder-image"
	SEEDER_CPU_REQUESTS     = "seeder-cpu-requests"
	SEEDER_CPU_LIMITS       = "seeder-cpu-limits"
	TRANSCODER_IMAGE        = "transcoder-image"
	TRANSCODER_CPU_REQUESTS = "transcoder-cpu-requests"
	TRANSCODER_CPU_LIMITS   = "transcoder-cpu-limits"
)

func RegisterConnectionConfigFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   SEEDER_IMAGE,
		Usage:  "Seeder image",
		Value:  "webtor/torrent-web-seeder:latest",
		EnvVar: "SEEDER_IMAGE",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   SEEDER_CPU_REQUESTS,
		Usage:  "Seeder CPU Requests",
		Value:  "",
		EnvVar: "SEEDER_CPU_REQUESTS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   SEEDER_CPU_LIMITS,
		Usage:  "Seeder CPU Limits",
		Value:  "",
		EnvVar: "SEEDER_CPU_LIMITS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TRANSCODER_IMAGE,
		Usage:  "Transcoder image",
		Value:  "webtor/content-transcoder:latest",
		EnvVar: "TRANSCODER_IMAGE",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TRANSCODER_CPU_REQUESTS,
		Usage:  "Transcoder CPU Requests",
		Value:  "",
		EnvVar: "TRANSCODER_CPU_REQUESTS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TRANSCODER_CPU_LIMITS,
		Usage:  "Transcoder CPU Limits",
		Value:  "",
		EnvVar: "TRANSCODER_CPU_LIMITS",
	})
}

func NewConnectionsConfig(c *cli.Context) *ConnectionsConfig {
	return &ConnectionsConfig{
		"default": &ConnectionConfig{
			Name:           "Torrent Web Seeder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Name:         "seeder",
				Image:        c.String(SEEDER_IMAGE),
				CPURequests:  c.String(SEEDER_CPU_REQUESTS),
				CPULimits:    c.String(SEEDER_CPU_LIMITS),
				Grace:        JOB_GRACE,
				IgnoredPaths: []string{"/TorrentWebSeeder/StatStream"},
			},
		},
		"hls": &ConnectionConfig{
			Name:           "HLS Content Transcoder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Name:        "transcoder",
				Image:       c.String(TRANSCODER_IMAGE),
				CPURequests: c.String(TRANSCODER_CPU_REQUESTS),
				CPULimits:   c.String(TRANSCODER_CPU_LIMITS),
				Grace:       JOB_GRACE,
			},
		},
		"vtt": &ConnectionConfig{
			Name:           "VTT Converter",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "SRT2VTT",
			},
		},
		"ext": &ConnectionConfig{
			Name:           "External Proxy",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "EXTERNAL_PROXY",
			},
		},
		"arch": &ConnectionConfig{
			Name:           "ZIP Archiver",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TORRENT_ARCHIVER",
			},
		},
		"vi": &ConnectionConfig{
			Name:           "Video Info Provider",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "VIDEO_INFO",
			},
		},
		"abuse": &ConnectionConfig{
			Name:           "Abuse Store",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "ABUSE_STORE",
			},
		},
		"magnet2torrent": &ConnectionConfig{
			Name:           "Magnet2Torrent",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "MAGNET2TORRENT",
			},
		},
		"store": &ConnectionConfig{
			Name:           "Torrent Store",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TORRENT_STORE",
			},
		},
	}
}
