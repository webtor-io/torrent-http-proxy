# torrent-http-proxy

Special HTTP-proxy that has several features:

1. Routes requests to internal kubernetes resources (services/jobs).
2. Deploys kubernetes job on demand.

   For example if path `/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel%2FSintel.mp4` was called
   then a new [torrent-web-seeder](https://github.com/webtor-io/torrent-web-seeder) job will be started with injected environment
   variable `INFO_HASH=08ada5a7a6183aae1e09d831df6748d566095a10`. Proxy will wait until pod will be ready and then proxy requst to
   it with path `/Sintel%2FSintel.mp4`. All following requests will be proxied to this pod.

3. Grants HTTP-access to GRPC-services (including jobs)
4. Provides Token-authentication
5. Performs chaining of service calls (matryoshka-style)

   For example `/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel%2FSintel.mp4~hls/index.m3u8` will be processed with following steps:
   
   1. Proxy deploys [content-transcoder](https://github.com/webtor-io/content-transcoder) job with injected environment variable `SOURCE_URL=%PROXY_URL%/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel%2FSintel.mp4`. `~hls` is the keyword that indicates what
   job or service should be invoked.
   2. [content-transcoder](https://github.com/webtor-io/content-transcoder) requests `SOURCE_URL` for transcoding.
   3. Proxy deploys [torrent-web-seeder](https://github.com/webtor-io/torrent-web-seeder) job with injected environment variable `INFO_HASH=08ada5a7a6183aae1e09d831df6748d566095a10`.
   4. Proxy serves `/index.m3u8` from [content-transcoder](https://github.com/webtor-io/content-transcoder)

   There might be more services in chain. There is no limitation.

## Server usage

```
% ./torrent-http-proxy help
NAME:
   torrent-http-proxy - Proxies all the things

USAGE:
   torrent-http-proxy [global options] command [command options] [arguments...]

VERSION:
   0.0.1

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --host value                     listening host
   --port value                     http listening port (default: 8080)
   --jwt-secret value               JWT Secret [$SECRET]
   --redis-host value               redis host (default: "localhost") [$REDIS_MASTER_SERVICE_HOST, $ REDIS_SERVICE_HOST]
   --redis-port value               redis port (default: 6379) [$REDIS_MASTER_SERVICE_PORT, $ REDIS_SERVICE_PORT]
   --job-node-affinity-key value    Node Affinity Key [$JOB_NODE_AFFINITY_KEY]
   --job-node-affinity-value value  Node Affinity Key [$JOB_NODE_AFFINITY_VALUE]
   --job-namespace value            Job namespace (default: "webtor") [$JOB_NAMESPACE]
   --probe-host value               probe listening host
   --probe-port value               probe listening port (default: 8081)
   --help, -h                       show help
   --version, -v                    print the version
```