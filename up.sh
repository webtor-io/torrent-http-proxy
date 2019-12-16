docker build . -t torrent-http-proxy:latest &&
telepresence --new-deployment torrent-http-proxy-debug --expose 8080 --docker-run -p 8080:8080 -v $HOME/.kube:/.kube --rm -it torrent-http-proxy:latest
