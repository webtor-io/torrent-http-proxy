docker build . -t torrent-http-proxy:latest &&
telepresence --new-deployment torrent-http-proxy-debug --expose 8080 --expose 8081 --expose 50051 --docker-run -p 8080:8080 -p 8081:8081 -p 50051:50051 -v $HOME/.kube:/root/.kube -v $HOME/.torrent-http-proxy:/etc/config --rm -it torrent-http-proxy:latest
