module github.com/webtor-io/torrent-http-proxy

go 1.15

require (
	github.com/bsm/redislock v0.4.0
	github.com/dgrijalva/jwt-go v3.2.0+incompatible
	github.com/gorilla/websocket v1.4.1 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware v1.0.1-0.20190118093823-f849b5445de4
	github.com/improbable-eng/grpc-web v0.11.0
	github.com/joonix/log v0.0.0-20190524090622-13fe31bbdd7a
	github.com/mwitkow/grpc-proxy v0.0.0-20181017164139-0f1106ef9c76
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.9.0
	github.com/prometheus/common v0.15.0
	github.com/rs/cors v1.7.0 // indirect
	github.com/sirupsen/logrus v1.6.0
	github.com/spf13/viper v1.6.2 // indirect
	github.com/urfave/cli v1.22.1
	github.com/webtor-io/common-services v0.0.0-20200315170226-d438614cbc09
	google.golang.org/grpc v1.26.0
	gopkg.in/yaml.v2 v2.3.0
	k8s.io/api v0.0.0-20191115135540-bbc9463b57e5
	k8s.io/apimachinery v0.0.0-20191115015347-3c7067801da2
	k8s.io/client-go v0.0.0-20190620085101-78d2af792bab
)
