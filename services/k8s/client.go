package k8s

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Client struct {
	cl     *kubernetes.Clientset
	inited bool
	err    error
	mux    sync.Mutex
}

func NewClient() *Client {
	return &Client{}
}

func (s *Client) get() (*kubernetes.Clientset, error) {
	kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	log.Infof("checking local kubeconfig path=%s", kubeconfig)
	var config *rest.Config
	if _, err := os.Stat(kubeconfig); err == nil {
		log.WithField("kubeconfig", kubeconfig).Info("loading config from file (local mode)")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, errors.Wrap(err, "failed to make config")
		}
	} else {
		log.Info("loading config from cluster (cluster mode)")
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, errors.Wrap(err, "failed to make config")
		}
	}
	config.Burst = 100
	config.QPS = -1
	return kubernetes.NewForConfig(config)
}

func (s *Client) Get() (*kubernetes.Clientset, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.cl, s.err
	}
	s.cl, s.err = s.get()
	s.inited = true
	return s.cl, s.err
}
