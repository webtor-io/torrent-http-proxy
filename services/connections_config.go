package services

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

func NewConnectionsConfig() *ConnectionsConfig {
	return &ConnectionsConfig{
		"default": &ConnectionConfig{
			Name:           "Torrent Web Seeder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Name:         "seeder",
				Image:        "gcr.io/vibrant-arcanum-201111/torrent-web-seeder:1cc43a28cfa3ca672b1c4c49ffe21881815c1237",
				CPURequests:  "75m",
				CPULimits:    "200m",
				Grace:        JOB_GRACE,
				IgnoredPaths: []string{"/TorrentWebSeeder/StatStream"},
			},
		},
		"hls": &ConnectionConfig{
			Name:           "HLS Content Transcoder",
			ConnectionType: ConnectionType_JOB,
			JobConfig: JobConfig{
				Name:        "transcoder",
				Image:       "gcr.io/vibrant-arcanum-201111/content-transcoder:af16e6458defdd73cb462ca45000550560617d6b",
				CPURequests: "100m",
				CPULimits:   "200m",
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
