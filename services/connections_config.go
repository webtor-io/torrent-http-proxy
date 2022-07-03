package services

import "github.com/urfave/cli"

type ConnectionType int

const (
	ConnectionType_SERVICE ConnectionType = 0
	ConnectionType_JOB     ConnectionType = 1
)

type JobType string

const (
	JobType_TRANSCODER JobType = "transcoder"
	JobType_SEEDER     JobType = "seeder"
)

type ServiceConfig struct {
	EnvName string
	Headers map[string]string
}

type JobConfig struct {
	Type                               JobType
	Name                               string
	Image                              string
	CPURequests                        string
	CPULimits                          string
	MemoryRequests                     string
	MemoryLimits                       string
	Grace                              int
	IgnoredPaths                       []string
	UseSnapshot                        string
	ToCompletion                       bool
	SnapshotStartFullDownloadThreshold float64
	SnapshotStartThreshold             float64
	SnapshotDownloadRatio              float64
	SnapshotTorrentSizeLimit           int64
	AWSAccessKeyID                     string
	AWSSecretAccessKey                 string
	AWSEndpoint                        string
	AWSRegion                          string
	AWSBucket                          string
	AWSBucketSpread                    string
	AWSNoSSL                           string
	RequestAffinity                    bool
	AffinityKey                        string
	AffinityValue                      string
	Env                                map[string]string
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
	for k := range map[string]*ConnectionConfig(s) {
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
	JOB_PREFIX                             = "job-prefix"
	SEEDER_IMAGE                           = "seeder-image"
	SEEDER_CPU_REQUESTS                    = "seeder-cpu-requests"
	SEEDER_CPU_LIMITS                      = "seeder-cpu-limits"
	SEEDER_MEMORY_REQUESTS                 = "seeder-memory-requests"
	SEEDER_MEMORY_LIMITS                   = "seeder-memory-limits"
	SEEDER_GRACE                           = "seeder-grace"
	SEEDER_AFFINITY_KEY                    = "seeder-affinity-key"
	SEEDER_AFFINITY_VALUE                  = "seeder-affinity-value"
	SEEDER_REQUEST_AFFINITY                = "seeder-request-affinity"
	TRANSCODER_IMAGE                       = "transcoder-image"
	TRANSCODER_CPU_REQUESTS                = "transcoder-cpu-requests"
	TRANSCODER_CPU_LIMITS                  = "transcoder-cpu-limits"
	TRANSCODER_MEMORY_REQUESTS             = "transcoder-memory-requests"
	TRANSCODER_MEMORY_LIMITS               = "transcoder-memory-limits"
	TRANSCODER_GRACE                       = "transcoder-grace"
	TRANSCODER_AFFINITY_KEY                = "transcoder-affinity-key"
	TRANSCODER_AFFINITY_VALUE              = "transcoder-affinity-value"
	TRANSCODER_REQUEST_AFFINITY            = "transcoder-request-affinity"
	MB_TRANSCODER_CPU_REQUESTS             = "mb-transcoder-cpu-requests"
	MB_TRANSCODER_CPU_LIMITS               = "mb-transcoder-cpu-limits"
	MB_TRANSCODER_MEMORY_REQUESTS          = "mb-transcoder-memory-requests"
	MB_TRANSCODER_MEMORY_LIMITS            = "mb-transcoder-memory-limits"
	MB_TRANSCODER_AFFINITY_KEY             = "mb-transcoder-affinity-key"
	MB_TRANSCODER_AFFINITY_VALUE           = "mb-transcoder-affinity-value"
	USE_SNAPSHOT                           = "use-snapshot"
	SNAPSHOT_START_THRESHOLD               = "snapshot-start-threshold"
	SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD = "snapshot-start-full-download-threshold"
	SNAPSHOT_DOWNLOAD_RATIO                = "snapshot-download-ratio"
	SNAPSHOT_TORRENT_SIZE_LIMIT            = "snapshot-torrent-size-limit"
	AWS_ACCESS_KEY_ID                      = "aws-access-key-id"
	AWS_SECRET_ACCESS_KEY                  = "aws-secret-access-key"
	AWS_BUCKET                             = "aws-bucket"
	AWS_BUCKET_SPREAD                      = "aws-bucket-spread"
	AWS_NO_SSL                             = "aws-no-ssl"
	AWS_REGION                             = "aws-region"
	AWS_ENDPOINT                           = "aws-endpoint"
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
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   SEEDER_MEMORY_REQUESTS,
		Usage:  "Seeder Memory Requests",
		Value:  "",
		EnvVar: "SEEDER_MEMORY_REQUESTS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   SEEDER_MEMORY_LIMITS,
		Usage:  "Seeder Memory Limits",
		Value:  "",
		EnvVar: "SEEDER_MEMORY_LIMITS",
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
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TRANSCODER_MEMORY_REQUESTS,
		Usage:  "Transcoder Memory Requests",
		Value:  "",
		EnvVar: "TRANSCODER_MEMORY_REQUESTS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TRANSCODER_MEMORY_LIMITS,
		Usage:  "Transcoder Memory Limits",
		Value:  "",
		EnvVar: "TRANSCODER_MEMORY_LIMITS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MB_TRANSCODER_CPU_REQUESTS,
		Usage:  "Multibitrate Transcoder CPU Requests",
		Value:  "",
		EnvVar: "MB_TRANSCODER_CPU_REQUESTS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MB_TRANSCODER_CPU_LIMITS,
		Usage:  "Multibitrate Transcoder CPU Limits",
		Value:  "",
		EnvVar: "MB_TRANSCODER_CPU_LIMITS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MB_TRANSCODER_MEMORY_REQUESTS,
		Usage:  "Multibitrate Transcoder Memory Requests",
		Value:  "",
		EnvVar: "MB_TRANSCODER_MEMORY_REQUESTS",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MB_TRANSCODER_MEMORY_LIMITS,
		Usage:  "Multibitrate Transcoder Memory Limits",
		Value:  "",
		EnvVar: "MB_TRANSCODER_MEMORY_LIMITS",
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
	c.Flags = append(c.Flags, cli.Float64Flag{
		Name:   SNAPSHOT_START_THRESHOLD,
		Value:  0.5,
		EnvVar: "SNAPSHOT_START_THRESHOLD",
	})
	c.Flags = append(c.Flags, cli.Float64Flag{
		Name:   SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD,
		Value:  0.75,
		EnvVar: "SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD",
	})
	c.Flags = append(c.Flags, cli.Float64Flag{
		Name:   SNAPSHOT_DOWNLOAD_RATIO,
		Value:  2.0,
		EnvVar: "SNAPSHOT_DOWNLOAD_RATIO",
	})
	c.Flags = append(c.Flags, cli.Int64Flag{
		Name:   SNAPSHOT_TORRENT_SIZE_LIMIT,
		Value:  10,
		EnvVar: "SNAPSHOT_TORRENT_SIZE_LIMIT",
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
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   AWS_BUCKET_SPREAD,
		EnvVar: "AWS_BUCKET_SPREAD",
	})
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   AWS_NO_SSL,
		EnvVar: "AWS_NO_SSL",
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
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   SEEDER_AFFINITY_KEY,
		Usage:  "Seeder Affinity Key",
		Value:  "",
		EnvVar: "SEEDER_AFFINITY_KEY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   SEEDER_AFFINITY_VALUE,
		Usage:  "Seeder Affinity Value",
		Value:  "",
		EnvVar: "SEEDER_AFFINITY_VALUE",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TRANSCODER_AFFINITY_KEY,
		Usage:  "Transcoder Affinity Key",
		Value:  "",
		EnvVar: "TRANSCODER_AFFINITY_KEY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TRANSCODER_AFFINITY_VALUE,
		Usage:  "Transcoder Affinity Value",
		Value:  "",
		EnvVar: "TRANSCODER_AFFINITY_VALUE",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MB_TRANSCODER_AFFINITY_KEY,
		Usage:  "Multibitrate Transcoder Affinity Key",
		Value:  "",
		EnvVar: "MB_TRANSCODER_AFFINITY_KEY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MB_TRANSCODER_AFFINITY_VALUE,
		Usage:  "Multibitrate Transcoder Affinity Value",
		Value:  "",
		EnvVar: "MB_TRANSCODER_AFFINITY_VALUE",
	})
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   SEEDER_REQUEST_AFFINITY,
		Usage:  "Seeder request affinity",
		EnvVar: "SEEDER_REQUEST_AFFINITY",
	})
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   TRANSCODER_REQUEST_AFFINITY,
		Usage:  "Transcoder request affinity",
		EnvVar: "TRANSCODER_REQUEST_AFFINITY",
	})
}

func NewConnectionsConfig(c *cli.Context) *ConnectionsConfig {
	return &ConnectionsConfig{
		"default": &ConnectionConfig{
			Name:           "torrent-web-seeder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Type:                               JobType_SEEDER,
				Name:                               c.String(JOB_PREFIX) + "seeder",
				Image:                              c.String(SEEDER_IMAGE),
				CPURequests:                        c.String(SEEDER_CPU_REQUESTS),
				CPULimits:                          c.String(SEEDER_CPU_LIMITS),
				MemoryRequests:                     c.String(SEEDER_MEMORY_REQUESTS),
				MemoryLimits:                       c.String(SEEDER_MEMORY_LIMITS),
				AWSAccessKeyID:                     c.String(AWS_ACCESS_KEY_ID),
				AWSSecretAccessKey:                 c.String(AWS_SECRET_ACCESS_KEY),
				AWSBucket:                          c.String(AWS_BUCKET),
				AWSBucketSpread:                    c.String(AWS_BUCKET_SPREAD),
				AWSNoSSL:                           c.String(AWS_NO_SSL),
				AWSEndpoint:                        c.String(AWS_ENDPOINT),
				AWSRegion:                          c.String(AWS_REGION),
				UseSnapshot:                        c.String(USE_SNAPSHOT),
				SnapshotStartThreshold:             c.Float64(SNAPSHOT_START_THRESHOLD),
				SnapshotStartFullDownloadThreshold: c.Float64(SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD),
				SnapshotDownloadRatio:              c.Float64(SNAPSHOT_DOWNLOAD_RATIO),
				SnapshotTorrentSizeLimit:           c.Int64(SNAPSHOT_TORRENT_SIZE_LIMIT),
				Grace:                              c.Int(SEEDER_GRACE),
				IgnoredPaths:                       []string{"/TorrentWebSeeder/StatStream"},
				RequestAffinity:                    c.Bool(SEEDER_REQUEST_AFFINITY),
				AffinityKey:                        c.String(SEEDER_AFFINITY_KEY),
				AffinityValue:                      c.String(SEEDER_AFFINITY_VALUE),
			},
		},
		// Single online content transcoder
		"hls": &ConnectionConfig{
			Name:           "content-transcoder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Type:                     JobType_TRANSCODER,
				Name:                     c.String(JOB_PREFIX) + "transcoder",
				Image:                    c.String(TRANSCODER_IMAGE),
				CPURequests:              c.String(TRANSCODER_CPU_REQUESTS),
				CPULimits:                c.String(TRANSCODER_CPU_LIMITS),
				MemoryRequests:           c.String(TRANSCODER_MEMORY_REQUESTS),
				MemoryLimits:             c.String(TRANSCODER_MEMORY_LIMITS),
				AWSAccessKeyID:           c.String(AWS_ACCESS_KEY_ID),
				AWSSecretAccessKey:       c.String(AWS_SECRET_ACCESS_KEY),
				AWSBucket:                c.String(AWS_BUCKET),
				AWSBucketSpread:          c.String(AWS_BUCKET_SPREAD),
				AWSNoSSL:                 c.String(AWS_NO_SSL),
				AWSEndpoint:              c.String(AWS_ENDPOINT),
				AWSRegion:                c.String(AWS_REGION),
				UseSnapshot:              c.String(USE_SNAPSHOT),
				SnapshotDownloadRatio:    c.Float64(SNAPSHOT_DOWNLOAD_RATIO),
				SnapshotTorrentSizeLimit: c.Int64(SNAPSHOT_TORRENT_SIZE_LIMIT),
				Grace:                    c.Int(TRANSCODER_GRACE),
				RequestAffinity:          c.Bool(TRANSCODER_REQUEST_AFFINITY),
				AffinityKey:              c.String(TRANSCODER_AFFINITY_KEY),
				AffinityValue:            c.String(TRANSCODER_AFFINITY_VALUE),
			},
		},
		// Multibitrate background content transcoder
		"mhls": &ConnectionConfig{
			Name:           "mb-content-transcoder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Type:                     JobType_TRANSCODER,
				Name:                     c.String(JOB_PREFIX) + "mb-transcoder",
				Image:                    c.String(TRANSCODER_IMAGE),
				CPURequests:              c.String(MB_TRANSCODER_CPU_REQUESTS),
				CPULimits:                c.String(MB_TRANSCODER_CPU_LIMITS),
				MemoryRequests:           c.String(MB_TRANSCODER_MEMORY_REQUESTS),
				MemoryLimits:             c.String(MB_TRANSCODER_MEMORY_LIMITS),
				AWSAccessKeyID:           c.String(AWS_ACCESS_KEY_ID),
				AWSSecretAccessKey:       c.String(AWS_SECRET_ACCESS_KEY),
				AWSBucket:                c.String(AWS_BUCKET),
				AWSBucketSpread:          c.String(AWS_BUCKET_SPREAD),
				AWSNoSSL:                 c.String(AWS_NO_SSL),
				AWSEndpoint:              c.String(AWS_ENDPOINT),
				AWSRegion:                c.String(AWS_REGION),
				UseSnapshot:              c.String(USE_SNAPSHOT),
				ToCompletion:             true,
				SnapshotDownloadRatio:    0,
				SnapshotTorrentSizeLimit: c.Int64(SNAPSHOT_TORRENT_SIZE_LIMIT),
				Grace:                    0,
				RequestAffinity:          false,
				AffinityKey:              c.String(MB_TRANSCODER_AFFINITY_KEY),
				AffinityValue:            c.String(MB_TRANSCODER_AFFINITY_VALUE),
				Env: map[string]string{
					"STREAM_MODE": "multibitrate",
					"KEY_PREFIX":  "mb-transcoder",
				},
			},
		},
		"mhlsp": &ConnectionConfig{
			Name:           "mb-content-transcoder-pooler",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "MB_CONTENT_TRANSCODER_POOLER",
			},
		},
		"trc": &ConnectionConfig{
			Name:           "transcode-web-cache",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TRANSCODE_WEB_CACHE",
			},
		},
		"mtrc": &ConnectionConfig{
			Name:           "mb-transcode-web-cache",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TRANSCODE_WEB_CACHE",
				Headers: map[string]string{
					"X-Key-Prefix": "mb-transcoder",
				},
			},
		},
		"vtt": &ConnectionConfig{
			Name:           "srt2vtt",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "SRT2VTT",
			},
		},
		"cp": &ConnectionConfig{
			Name:           "content-prober",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "CONTENT_PROBER",
			},
		},
		"ext": &ConnectionConfig{
			Name:           "external-proxy",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "EXTERNAL_PROXY",
			},
		},
		"arch": &ConnectionConfig{
			Name:           "torrent-archiver",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TORRENT_ARCHIVER",
			},
		},
		"vi": &ConnectionConfig{
			Name:           "video-info",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "VIDEO_INFO",
			},
		},
		"vtg": &ConnectionConfig{
			Name:           "video-thumbnails-generator",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "VIDEO_THUMBNAILS_GENERATOR",
			},
		},
		"rest": &ConnectionConfig{
			Name:           "rest",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "REST_API",
			},
		},
		"it": &ConnectionConfig{
			Name:           "image-transformer",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "IMAGE_TRANSFORMER",
			},
		},
		"tracker": &ConnectionConfig{
			Name:           "webtorrent-tracker",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "WEBTORRENT_TRACKER",
			},
		},
		"tc": &ConnectionConfig{
			Name:           "torrent-web-cache",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TORRENT_WEB_CACHE",
			},
		},
		"abuse": &ConnectionConfig{
			Name:           "abuse-store",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "ABUSE_STORE",
			},
		},
		"magnet2torrent": &ConnectionConfig{
			Name:           "magnet2torrent",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "MAGNET2TORRENT",
			},
		},
		"store": &ConnectionConfig{
			Name:           "torrent-store",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TORRENT_STORE",
			},
		},
		"store-new": &ConnectionConfig{
			Name:           "torrent-store-new",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "TORRENT_STORE_NEW",
			},
		},
		"vod": &ConnectionConfig{
			Name:           "nginx-vod",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "NGINX_VOD",
			},
		},
		"dp": &ConnectionConfig{
			Name:           "download-progress",
			ConnectionType: ConnectionType_SERVICE,
			ServiceConfig: ServiceConfig{
				EnvName: "DOWNLOAD_PROGRESS",
			},
		},
	}
}
