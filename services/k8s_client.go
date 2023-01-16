package services

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

type K8SClient struct {
	cl     *kubernetes.Clientset
	inited bool
	err    error
	mux    sync.Mutex
}

func NewK8SClient() *K8SClient {
	return &K8SClient{inited: false}
}

func (s *K8SClient) get() (*kubernetes.Clientset, error) {
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
	return kubernetes.NewForConfig(config)
}

func (s *K8SClient) Get() (*kubernetes.Clientset, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.cl, s.err
	}
	s.cl, s.err = s.get()
	s.inited = true
	return s.cl, s.err
}
