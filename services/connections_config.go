package services

import "github.com/urfave/cli"

type ConnectionType int

const (
	ConnectionType_SERVICE ConnectionType = 0
	ConnectionType_JOB     ConnectionType = 1
)

type ServiceConfig struct {
	EnvName string
}

type JobConfig struct {
	Name               string
	Image              string
	CPURequests        string
	CPULimits          string
	Grace              int
	IgnoredPaths       []string
	UseSnapshot        string
	ResticPassword     string
	ResticRepository   string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSEndpoint        string
	AWSRegion          string
	AWSBucket          string
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
	JOB_PREFIX              = "job-prefix"
	SEEDER_IMAGE            = "seeder-image"
	SEEDER_CPU_REQUESTS     = "seeder-cpu-requests"
	SEEDER_CPU_LIMITS       = "seeder-cpu-limits"
	SEEDER_GRACE            = "seeder-grace"
	TRANSCODER_IMAGE        = "transcoder-image"
	TRANSCODER_CPU_REQUESTS = "transcoder-cpu-requests"
	TRANSCODER_CPU_LIMITS   = "transcoder-cpu-limits"
	TRANSCODER_GRACE        = "transcoder-grace"
	USE_SNAPSHOT            = "use-snapshot"
	AWS_ACCESS_KEY_ID       = "aws-access-key-id"
	AWS_SECRET_ACCESS_KEY   = "aws-secret-access-key"
	AWS_BUCKET              = "aws-bucket"
	AWS_REGION              = "aws-region"
	AWS_ENDPOINT            = "aws-endpoint"
)

func RegisterConnectionConfigFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   JOB_PREFIX,
		Usage:  "Job prefix",
		Value:  "",
		EnvVar: "JOB_PREFIX",
	})
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
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   SEEDER_GRACE,
		Usage:  "Seeder Grace (sec)",
		Value:  600,
		EnvVar: "SEEDER_GRACE",
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
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   TRANSCODER_GRACE,
		Usage:  "Transcoder Grace (sec)",
		Value:  600,
		EnvVar: "TRANSCODER_GRACE",
	})
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   USE_SNAPSHOT,
		Usage:  "use snapshot",
		EnvVar: "USE_SNAPSHOT",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_ACCESS_KEY_ID,
		Usage:  "AWS Access Key ID",
		Value:  "",
		EnvVar: "AWS_ACCESS_KEY_ID",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_SECRET_ACCESS_KEY,
		Usage:  "AWS Secret Access Key",
		Value:  "",
		EnvVar: "AWS_SECRET_ACCESS_KEY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_BUCKET,
		Usage:  "AWS Bucket",
		Value:  "",
		EnvVar: "AWS_BUCKET",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_ENDPOINT,
		Usage:  "AWS Endpoint",
		Value:  "",
		EnvVar: "AWS_ENDPOINT",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_REGION,
		Usage:  "AWS Region",
		Value:  "",
		EnvVar: "AWS_REGION",
	})
}

func NewConnectionsConfig(c *cli.Context) *ConnectionsConfig {
	return &ConnectionsConfig{
		"default": &ConnectionConfig{
			Name:           "Torrent Web Seeder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Name:               c.String(JOB_PREFIX) + "seeder",
				Image:              c.String(SEEDER_IMAGE),
				CPURequests:        c.String(SEEDER_CPU_REQUESTS),
				CPULimits:          c.String(SEEDER_CPU_LIMITS),
				AWSAccessKeyID:     c.String(AWS_ACCESS_KEY_ID),
				AWSSecretAccessKey: c.String(AWS_SECRET_ACCESS_KEY),
				AWSBucket:          c.String(AWS_BUCKET),
				AWSEndpoint:        c.String(AWS_ENDPOINT),
				AWSRegion:          c.String(AWS_REGION),
				UseSnapshot:        c.String(USE_SNAPSHOT),
				Grace:              c.Int(SEEDER_GRACE),
				IgnoredPaths:       []string{"/TorrentWebSeeder/StatStream"},
			},
		},
		"hls": &ConnectionConfig{
			Name:           "HLS Content Transcoder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Name:        c.String(JOB_PREFIX) + "transcoder",
				Image:       c.String(TRANSCODER_IMAGE),
				CPURequests: c.String(TRANSCODER_CPU_REQUESTS),
				CPULimits:   c.String(TRANSCODER_CPU_LIMITS),
				Grace:       c.Int(TRANSCODER_GRACE),
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
		"tc": &ConnectionConfig{
			Name:           "Torrent Web Cache",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TORRENT_WEB_CACHE",
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
