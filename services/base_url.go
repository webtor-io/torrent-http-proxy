package services

import (
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
)

func GetBaseURL() string {
	baseURL := ""
	if os.Getenv("RETRY_PROXY_SERVICE_HOST") != "" {
		baseURL = fmt.Sprintf("http://%v:%v", os.Getenv("RETRY_PROXY_SERVICE_HOST"), os.Getenv("RETRY_PROXY_SERVICE_PORT"))
	} else if os.Getenv("TORRENT_HTTP_PROXY_DEBUG_SERVICE_HOST") != "" {
		baseURL = fmt.Sprintf("http://%v:%v", os.Getenv("TORRENT_HTTP_PROXY_DEBUG_SERVICE_HOST"), os.Getenv("TORRENT_HTTP_PROXY_DEBUG_SERVICE_PORT"))
		log.Infof("setting debug baseURL=%s", baseURL)
	} else {
		baseURL = fmt.Sprintf("http://%v:%v", os.Getenv("TORRENT_HTTP_PROXY_SERVICE_HOST"), os.Getenv("TORRENT_HTTP_PROXY_SERVICE_PORT"))
	}
	return baseURL
}
