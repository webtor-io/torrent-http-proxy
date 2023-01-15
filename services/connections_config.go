package services

import (
	"strings"

	"github.com/urfave/cli"
)

type ConnectionType int

const (
	ConnectionTypeService ConnectionType = 0
	ConnectionTypeJob     ConnectionType = 1
)

type JobType string

const (
	JobTypeTranscoder JobType = "transcoder"
)

type ServiceConfig struct {
	Name            string
	Distribution    DISTRIBUTION
	PreferLocalNode bool
	Headers         map[string]string
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
	Labels                             map[string]string
	HTTPProxy                          string
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
	var res []string
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
	jobPrefixFlag                          = "job-prefix"
	seederImageFlag                        = "seeder-image"
	seederCPURequestsFlag                  = "seeder-cpu-requests"
	seederCPULimitsFlag                    = "seeder-cpu-limits"
	seederMemoryRequestsFlag               = "seeder-memory-requests"
	seederMemoryLimitsFlag                 = "seeder-memory-limits"
	seederGraceFlag                        = "seeder-grace"
	seederAffinityKeyFlag                  = "seeder-affinity-key"
	seederAffinityValueFlag                = "seeder-affinity-value"
	seederRequestAffinityFlag              = "seeder-request-affinity"
	seederLabelsFlag                       = "seeder-labels"
	seederHTTPProxyFlag                    = "seeder-http-proxy"
	transcoderImageFlag                    = "transcoder-image"
	transcoderCPURequestsFlag              = "transcoder-cpu-requests"
	transcoderCPULimitsFlag                = "transcoder-cpu-limits"
	transcoderMemoryRequestsFlag           = "transcoder-memory-requests"
	transcoderMemoryLimitsFlag             = "transcoder-memory-limits"
	transcoderGraceFlag                    = "transcoder-grace"
	transcoderAffinityKeyFlag              = "transcoder-affinity-key"
	transcoderAffinityValueFlag            = "transcoder-affinity-value"
	transcoderRequestAffinityFlag          = "transcoder-request-affinity"
	transcoderLabelsFlag                   = "transcoder-labels"
	mbTranscoderCPURequestsFlag            = "mb-transcoder-cpu-requests"
	mbTranscoderCPULimitsFlag              = "mb-transcoder-cpu-limits"
	mbTranscoderMemoryRequestsFlag         = "mb-transcoder-memory-requests"
	mbTranscoderMemoryLimitsFlag           = "mb-transcoder-memory-limits"
	mbTranscoderAffinityKeyFlag            = "mb-transcoder-affinity-key"
	mbTranscoderAffinityValueFlag          = "mb-transcoder-affinity-value"
	mbTranscoderLabelsFlag                 = "mb-transcoder-labels"
	useSnapshotFlag                        = "use-snapshot"
	snapshotStartThresholdFlag             = "snapshot-start-threshold"
	snapshotStartFullDownloadThresholdFlag = "snapshot-start-full-download-threshold"
	snapshotDownloadRatioFlag              = "snapshot-download-ratio"
	snapshotTorrentSizeLimitFlag           = "snapshot-torrent-size-limit"
	awsAccessKeyIDFlag                     = "aws-access-key-id"
	awsSecretAccessKeyFlag                 = "aws-secret-access-key"
	awsBucketFlag                          = "aws-bucket"
	awsBucketSpreadFlag                    = "aws-bucket-spread"
	awsNoSSLFlag                           = "aws-no-ssl"
	awsRegionFlag                          = "aws-region"
	awsEndpointFlag                        = "aws-endpoint"
)

func RegisterConnectionConfigFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   jobPrefixFlag,
			Usage:  "Job prefix",
			Value:  "",
			EnvVar: "JOB_PREFIX",
		},
		cli.StringFlag{
			Name:   seederImageFlag,
			Usage:  "Seeder image",
			Value:  "webtor/torrent-web-seeder:latest",
			EnvVar: "SEEDER_IMAGE",
		},
		cli.StringFlag{
			Name:   seederCPURequestsFlag,
			Usage:  "Seeder CPU Requests",
			Value:  "",
			EnvVar: "SEEDER_CPU_REQUESTS",
		},
		cli.StringFlag{
			Name:   seederCPULimitsFlag,
			Usage:  "Seeder CPU Limits",
			Value:  "",
			EnvVar: "SEEDER_CPU_LIMITS",
		},
		cli.StringFlag{
			Name:   seederMemoryRequestsFlag,
			Usage:  "Seeder Memory Requests",
			Value:  "",
			EnvVar: "SEEDER_MEMORY_REQUESTS",
		},
		cli.StringFlag{
			Name:   seederMemoryLimitsFlag,
			Usage:  "Seeder Memory Limits",
			Value:  "",
			EnvVar: "SEEDER_MEMORY_LIMITS",
		},
		cli.IntFlag{
			Name:   seederGraceFlag,
			Usage:  "Seeder Grace (sec)",
			Value:  600,
			EnvVar: "SEEDER_GRACE",
		},
		cli.StringFlag{
			Name:   transcoderImageFlag,
			Usage:  "Transcoder image",
			Value:  "webtor/content-transcoder:latest",
			EnvVar: "TRANSCODER_IMAGE",
		},
		cli.StringFlag{
			Name:   transcoderCPURequestsFlag,
			Usage:  "Transcoder CPU Requests",
			Value:  "",
			EnvVar: "TRANSCODER_CPU_REQUESTS",
		},
		cli.StringFlag{
			Name:   transcoderCPULimitsFlag,
			Usage:  "Transcoder CPU Limits",
			Value:  "",
			EnvVar: "TRANSCODER_CPU_LIMITS",
		},
		cli.StringFlag{
			Name:   transcoderMemoryRequestsFlag,
			Usage:  "Transcoder Memory Requests",
			Value:  "",
			EnvVar: "TRANSCODER_MEMORY_REQUESTS",
		},
		cli.StringFlag{
			Name:   transcoderMemoryLimitsFlag,
			Usage:  "Transcoder Memory Limits",
			Value:  "",
			EnvVar: "TRANSCODER_MEMORY_LIMITS",
		},
		cli.StringFlag{
			Name:   mbTranscoderCPURequestsFlag,
			Usage:  "Multibitrate Transcoder CPU Requests",
			Value:  "",
			EnvVar: "MB_TRANSCODER_CPU_REQUESTS",
		},
		cli.StringFlag{
			Name:   mbTranscoderCPULimitsFlag,
			Usage:  "Multibitrate Transcoder CPU Limits",
			Value:  "",
			EnvVar: "MB_TRANSCODER_CPU_LIMITS",
		},
		cli.StringFlag{
			Name:   mbTranscoderMemoryRequestsFlag,
			Usage:  "Multibitrate Transcoder Memory Requests",
			Value:  "",
			EnvVar: "MB_TRANSCODER_MEMORY_REQUESTS",
		},
		cli.StringFlag{
			Name:   mbTranscoderMemoryLimitsFlag,
			Usage:  "Multibitrate Transcoder Memory Limits",
			Value:  "",
			EnvVar: "MB_TRANSCODER_MEMORY_LIMITS",
		},
		cli.IntFlag{
			Name:   transcoderGraceFlag,
			Usage:  "Transcoder Grace (sec)",
			Value:  600,
			EnvVar: "TRANSCODER_GRACE",
		},
		cli.BoolFlag{
			Name:   useSnapshotFlag,
			Usage:  "use snapshot",
			EnvVar: "USE_SNAPSHOT",
		},
		cli.Float64Flag{
			Name:   snapshotStartThresholdFlag,
			Value:  0.5,
			EnvVar: "SNAPSHOT_START_THRESHOLD",
		},
		cli.Float64Flag{
			Name:   snapshotStartFullDownloadThresholdFlag,
			Value:  0.75,
			EnvVar: "SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD",
		},
		cli.Float64Flag{
			Name:   snapshotDownloadRatioFlag,
			Value:  2.0,
			EnvVar: "SNAPSHOT_DOWNLOAD_RATIO",
		},
		cli.Int64Flag{
			Name:   snapshotTorrentSizeLimitFlag,
			Value:  10,
			EnvVar: "SNAPSHOT_TORRENT_SIZE_LIMIT",
		},
		cli.StringFlag{
			Name:   awsAccessKeyIDFlag,
			Usage:  "AWS Access Key ID",
			Value:  "",
			EnvVar: "AWS_ACCESS_KEY_ID",
		},
		cli.StringFlag{
			Name:   awsSecretAccessKeyFlag,
			Usage:  "AWS Secret Access Key",
			Value:  "",
			EnvVar: "AWS_SECRET_ACCESS_KEY",
		},
		cli.StringFlag{
			Name:   awsBucketFlag,
			Usage:  "AWS Bucket",
			Value:  "",
			EnvVar: "AWS_BUCKET",
		},
		cli.BoolFlag{
			Name:   awsBucketSpreadFlag,
			EnvVar: "AWS_BUCKET_SPREAD",
		},
		cli.BoolFlag{
			Name:   awsNoSSLFlag,
			EnvVar: "AWS_NO_SSL",
		},
		cli.StringFlag{
			Name:   awsEndpointFlag,
			Usage:  "AWS Endpoint",
			Value:  "",
			EnvVar: "AWS_ENDPOINT",
		},
		cli.StringFlag{
			Name:   awsRegionFlag,
			Usage:  "AWS Region",
			Value:  "",
			EnvVar: "AWS_REGION",
		},
		cli.StringFlag{
			Name:   seederAffinityKeyFlag,
			Usage:  "Seeder Affinity Key",
			Value:  "",
			EnvVar: "SEEDER_AFFINITY_KEY",
		},
		cli.StringFlag{
			Name:   seederAffinityValueFlag,
			Usage:  "Seeder Affinity Value",
			Value:  "",
			EnvVar: "SEEDER_AFFINITY_VALUE",
		},
		cli.StringFlag{
			Name:   transcoderAffinityKeyFlag,
			Usage:  "Transcoder Affinity Key",
			Value:  "",
			EnvVar: "TRANSCODER_AFFINITY_KEY",
		},
		cli.StringFlag{
			Name:   transcoderAffinityValueFlag,
			Usage:  "Transcoder Affinity Value",
			Value:  "",
			EnvVar: "TRANSCODER_AFFINITY_VALUE",
		},
		cli.StringFlag{
			Name:   mbTranscoderAffinityKeyFlag,
			Usage:  "Multibitrate Transcoder Affinity Key",
			Value:  "",
			EnvVar: "MB_TRANSCODER_AFFINITY_KEY",
		},
		cli.StringFlag{
			Name:   mbTranscoderAffinityValueFlag,
			Usage:  "Multibitrate Transcoder Affinity Value",
			Value:  "",
			EnvVar: "MB_TRANSCODER_AFFINITY_VALUE",
		},
		cli.StringFlag{
			Name:   mbTranscoderLabelsFlag,
			Usage:  "Multibitrate Transcoder Labels",
			Value:  "",
			EnvVar: "MB_TRANSCODER_LABELS",
		},
		cli.BoolFlag{
			Name:   seederRequestAffinityFlag,
			Usage:  "Seeder request affinity",
			EnvVar: "SEEDER_REQUEST_AFFINITY",
		},
		cli.BoolFlag{
			Name:   transcoderRequestAffinityFlag,
			Usage:  "Transcoder request affinity",
			EnvVar: "TRANSCODER_REQUEST_AFFINITY",
		},
		cli.StringFlag{
			Name:   seederLabelsFlag,
			Usage:  "Seeder additional labels",
			EnvVar: "SEEDER_LABELS",
		},
		cli.StringFlag{
			Name:   seederHTTPProxyFlag,
			Usage:  "Seeder HTTP proxy",
			EnvVar: "SEEDER_HTTP_PROXY",
		},
		cli.StringFlag{
			Name:   transcoderLabelsFlag,
			Usage:  "Transcoder additional labels",
			EnvVar: "TRANSCODER_LABELS",
		},
	)
}

func processLabels(s string) map[string]string {
	r := map[string]string{}
	if s == "" {
		return r
	}
	for _, v := range strings.Split(s, ",") {
		vv := strings.Split(v, "=")
		if len(vv) == 2 {
			r[vv[0]] = vv[1]
		}
	}
	return r
}

func NewConnectionsConfig(c *cli.Context) *ConnectionsConfig {
	return &ConnectionsConfig{
		"default": &ConnectionConfig{
			Name:           "torrent-web-seeder",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name:            "torrent-web-seeder",
				Distribution:    NodeHash,
				PreferLocalNode: true,
			},
		},
		// Single online content transcoder
		"hls": &ConnectionConfig{
			Name:           "content-transcoder",
			ConnectionType: ConnectionTypeJob,
			JobConfig: JobConfig{
				Type:                     JobTypeTranscoder,
				Name:                     c.String(jobPrefixFlag) + "transcoder",
				Image:                    c.String(transcoderImageFlag),
				CPURequests:              c.String(transcoderCPURequestsFlag),
				CPULimits:                c.String(transcoderCPULimitsFlag),
				MemoryRequests:           c.String(transcoderMemoryRequestsFlag),
				MemoryLimits:             c.String(transcoderMemoryLimitsFlag),
				AWSAccessKeyID:           c.String(awsAccessKeyIDFlag),
				AWSSecretAccessKey:       c.String(awsSecretAccessKeyFlag),
				AWSBucket:                c.String(awsBucketFlag),
				AWSBucketSpread:          c.String(awsBucketSpreadFlag),
				AWSNoSSL:                 c.String(awsNoSSLFlag),
				AWSEndpoint:              c.String(awsEndpointFlag),
				AWSRegion:                c.String(awsRegionFlag),
				UseSnapshot:              c.String(useSnapshotFlag),
				SnapshotDownloadRatio:    c.Float64(snapshotDownloadRatioFlag),
				SnapshotTorrentSizeLimit: c.Int64(snapshotTorrentSizeLimitFlag),
				Grace:                    c.Int(transcoderGraceFlag),
				RequestAffinity:          c.Bool(transcoderRequestAffinityFlag),
				AffinityKey:              c.String(transcoderAffinityKeyFlag),
				AffinityValue:            c.String(transcoderAffinityValueFlag),
				Labels:                   processLabels(c.String(transcoderLabelsFlag)),
			},
		},
		// Multibitrate background content transcoder
		"mhls": &ConnectionConfig{
			Name:           "mb-content-transcoder",
			ConnectionType: ConnectionTypeJob,
			JobConfig: JobConfig{
				Type:                     JobTypeTranscoder,
				Name:                     c.String(jobPrefixFlag) + "mb-transcoder",
				Image:                    c.String(transcoderImageFlag),
				CPURequests:              c.String(mbTranscoderCPURequestsFlag),
				CPULimits:                c.String(mbTranscoderCPULimitsFlag),
				MemoryRequests:           c.String(mbTranscoderMemoryRequestsFlag),
				MemoryLimits:             c.String(mbTranscoderMemoryLimitsFlag),
				AWSAccessKeyID:           c.String(awsAccessKeyIDFlag),
				AWSSecretAccessKey:       c.String(awsSecretAccessKeyFlag),
				AWSBucket:                c.String(awsBucketFlag),
				AWSBucketSpread:          c.String(awsBucketSpreadFlag),
				AWSNoSSL:                 c.String(awsNoSSLFlag),
				AWSEndpoint:              c.String(awsEndpointFlag),
				AWSRegion:                c.String(awsRegionFlag),
				UseSnapshot:              c.String(useSnapshotFlag),
				ToCompletion:             true,
				SnapshotDownloadRatio:    0,
				SnapshotTorrentSizeLimit: c.Int64(snapshotTorrentSizeLimitFlag),
				Grace:                    0,
				RequestAffinity:          false,
				AffinityKey:              c.String(mbTranscoderAffinityKeyFlag),
				AffinityValue:            c.String(mbTranscoderAffinityValueFlag),
				Env: map[string]string{
					"STREAM_MODE": "multibitrate",
					"KEY_PREFIX":  "mb-transcoder",
				},
				Labels: processLabels(c.String(mbTranscoderLabelsFlag)),
			},
		},
		"mhlsp": &ConnectionConfig{
			Name:           "mb-content-transcoder-pooler",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "mb-content-transcoder-pooler",
			},
		},
		"trc": &ConnectionConfig{
			Name:           "transcode-web-cache",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name:            "transcode-web-cache",
				Distribution:    NodeHash,
				PreferLocalNode: true,
			},
		},
		"mtrc": &ConnectionConfig{
			Name:           "mb-transcode-web-cache",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "transcode-web-cache",
				Headers: map[string]string{
					"X-Key-Prefix": "mb-transcoder",
				},
				Distribution:    NodeHash,
				PreferLocalNode: true,
			},
		},
		"vtt": &ConnectionConfig{
			Name:           "srt2vtt",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "srt2vtt",
			},
		},
		"cp": &ConnectionConfig{
			Name:           "content-prober",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "content-prober",
			},
		},
		"ext": &ConnectionConfig{
			Name:           "external-proxy",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "external-proxy",
			},
		},
		"arch": &ConnectionConfig{
			Name:           "torrent-archiver",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name:            "torrent-archiver",
				Distribution:    NodeHash,
				PreferLocalNode: true,
			},
		},
		"vi": &ConnectionConfig{
			Name:           "video-info",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "video-info",
			},
		},
		"vtg": &ConnectionConfig{
			Name:           "video-thumbnails-generator",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "video-thumbnails-generator",
			},
		},
		"rest": &ConnectionConfig{
			Name:           "rest",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "rest",
			},
		},
		"it": &ConnectionConfig{
			Name:           "image-transformer",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "image-transformer",
			},
		},
		"tracker": &ConnectionConfig{
			Name:           "webtorrent-tracker",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "webtorrent-tracker",
			},
		},
		"tc": &ConnectionConfig{
			Name:           "torrent-web-cache",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name:            "torrent-web-cache",
				Distribution:    NodeHash,
				PreferLocalNode: true,
			},
		},
		"abuse": &ConnectionConfig{
			Name:           "abuse-store",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "abuse-store",
			},
		},
		"magnet2torrent": &ConnectionConfig{
			Name:           "magnet2torrent",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "magnet2torrent",
			},
		},
		"store": &ConnectionConfig{
			Name:           "torrent-store",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name: "torrent-store",
			},
		},
		"vod": &ConnectionConfig{
			Name:           "nginx-vod",
			ConnectionType: ConnectionTypeService,
			ServiceConfig: ServiceConfig{
				Name:            "nginx-vod",
				Distribution:    NodeHash,
				PreferLocalNode: true,
			},
		},
		// "dp": &ConnectionConfig{
		// 	Name:           "download-progress",
		// 	ConnectionType: ConnectionTypeService,
		// 	ServiceConfig: ServiceConfig{
		// 		Name:    "download-progress",
		// 		EnvName: "DOWNLOAD_PROGRESS",
		// 	},
		// },
	}
}
