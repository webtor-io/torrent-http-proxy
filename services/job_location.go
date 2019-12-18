package services

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bsm/redislock"
	"github.com/urfave/cli"

	"k8s.io/apimachinery/pkg/fields"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	PORT_HTTP               = 8080
	PORT_PROBE              = 8081
	PORT_GRPC               = 50051
	HEALTH_CHECK_TIMEOUT    = 3
	HEALTH_CHECK_INTERVAL   = 5
	POD_LOCK_DURATION       = 5
	POD_LOCK_STANDBY        = 1
	POD_LIVENESS_PATH       = "/liveness"
	POD_READINESS_PATH      = "/readiness"
	JOB_NODE_AFFINITY_KEY   = "job-node-affinity-key"
	JOB_NODE_AFFINITY_VALUE = "job-node-affinity-value"
	JOB_NAMESPACE           = "job-namespace"
)

type JobLocation struct {
	id        string
	cl        *K8SClient
	loc       *Location
	cfg       *JobConfig
	pod       *corev1.Pod
	params    *InitParams
	inited    bool
	err       error
	mux       sync.Mutex
	logger    *logrus.Entry
	l         *Locker
	naKey     string
	naVal     string
	namespace string
}

func RegisterJobFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   JOB_NODE_AFFINITY_KEY,
		Usage:  "Node Affinity Key",
		Value:  "",
		EnvVar: "JOB_NODE_AFFINITY_KEY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   JOB_NODE_AFFINITY_VALUE,
		Usage:  "Node Affinity Value",
		Value:  "",
		EnvVar: "JOB_NODE_AFFINITY_VALUE",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   JOB_NAMESPACE,
		Usage:  "Job namespace",
		Value:  "webtor",
		EnvVar: "JOB_NAMESPACE",
	})
}

func NewJobLocation(c *cli.Context, cfg *JobConfig, params *InitParams, cl *K8SClient, logger *logrus.Entry, l *Locker) *JobLocation {
	id := MakeJobID(cfg, params)
	return &JobLocation{cfg: cfg, params: params, cl: cl, id: id, inited: false,
		logger: logger, l: l, naKey: c.String(JOB_NODE_AFFINITY_KEY), naVal: c.String(JOB_NODE_AFFINITY_VALUE), namespace: c.String(JOB_NAMESPACE)}
}

func isPodFinished(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.ContainersReady &&
			cond.Status == corev1.ConditionTrue &&
			pod.Status.Phase == corev1.PodRunning {
			return true
		}
	}
	return false
}

func podToLocation(pod *corev1.Pod) *Location {
	return &Location{
		IP:     net.ParseIP(pod.Status.PodIP),
		Active: true,
		Ports: Ports{
			HTTP:  PORT_HTTP,
			GRPC:  PORT_GRPC,
			Probe: PORT_PROBE,
		},
		Unavailable: false,
	}
}

func (s *JobLocation) WaitFinish() error {
	if s.pod == nil {
		return errors.Errorf("No pod to wait for finish")
	}
	cl, err := s.cl.Get()
	if err != nil {
		return errors.Wrap(err, "Failed to get K8S client")
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", s.pod.Name).String(),
	})
	if err != nil {
		return errors.Wrap(err, "Failed to create watcher")
	}
	defer watcher.Stop()
	watchSuccess := make(chan *corev1.Pod)
	go func() {
		for event := range watcher.ResultChan() {
			pod := event.Object.(*corev1.Pod)

			s.logger.Info(pod.Status.Phase)

			if isPodFinished(pod) {
				s.logger.Info("Pod finished!")
				watchSuccess <- pod
			}
		}
	}()
	httpHealthCheck := make(chan error)
	go func() {
		netClient := &http.Client{
			Timeout: time.Second * HEALTH_CHECK_TIMEOUT,
		}
		for {
			url := fmt.Sprintf("http://%s:%d%s", s.pod.Status.PodIP, PORT_PROBE, POD_LIVENESS_PATH)
			// s.logger.Infof("Checking url=%s", url)
			res, err := netClient.Get(url)
			if err != nil {
				s.logger.WithError(err).Error("Failed to check pod status")
				httpHealthCheck <- err
				return
			}
			// s.logger.Infof("Got http status=%d", res.StatusCode)
			if res.StatusCode != http.StatusOK {
				httpHealthCheck <- nil
				return
			}
			time.Sleep(time.Second * HEALTH_CHECK_INTERVAL)
		}
	}()
	select {
	case <-watchSuccess:
		s.logger.Info("Pod finished")
	case <-httpHealthCheck:
		s.logger.Info("Job health check failed")
	}
	return nil
}

func (s *JobLocation) WaitReady(timeout <-chan time.Time) (*corev1.Pod, error) {
	if s.pod == nil {
		return nil, errors.Errorf("No pod to wait for ready")
	}
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get K8S client")
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", s.pod.Name).String(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create watcher")
	}
	defer watcher.Stop()
	watchSuccess := make(chan *corev1.Pod)
	go func() {
		for event := range watcher.ResultChan() {
			pod := event.Object.(*corev1.Pod)

			if isPodReady(pod) {
				s.logger.Info("Pod finished!")
				watchSuccess <- pod
			}
		}
	}()
	select {
	case <-timeout:
		return nil, errors.Errorf("Got timeout")
	case p := <-watchSuccess:
		return p, nil
	}
}

func (s *JobLocation) makeResources() corev1.ResourceRequirements {
	res := corev1.ResourceRequirements{}
	if s.cfg.CPURequests != "" {
		res.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(s.cfg.CPURequests),
		}
	}
	if s.cfg.CPULimits != "" {
		res.Limits = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(s.cfg.CPURequests),
		}
	}
	return res

}

func (s *JobLocation) makeAffinity() []corev1.PreferredSchedulingTerm {
	aff := []corev1.PreferredSchedulingTerm{}
	if s.naKey != "" && s.naVal != "" {
		aff = append(aff, corev1.PreferredSchedulingTerm{
			Weight: 1,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{
						Key:      s.naKey,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{s.naVal},
					},
				},
			},
		})
	}
	return aff
}

func (s *JobLocation) get() (*Location, error) {
	s.logger.Info("Job initialization started")
	start := time.Now()
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get K8S client")
	}
	wasLocked := false
	l, err := s.l.Get().Obtain(s.id, time.Second*POD_LOCK_DURATION, nil)
	if err == redislock.ErrNotObtained {
		s.logger.Warn("Failed to obtain lock")
		wasLocked = true
		time.Sleep(time.Second * POD_LOCK_STANDBY)
	} else if err != nil {
		return nil, errors.Wrap(err, "Failed to set lock")
	} else {
		defer l.Release()
	}
	timeout := time.After(5 * time.Minute)
	pods, err := cl.CoreV1().Pods(s.namespace).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-id=%v", s.id),
	})
	if err != nil {
		return nil, errors.Wrap(err, "Failed to find active job")
	}
	if len(pods.Items) > 0 {
		p := &pods.Items[0]
		s.pod = p
		if isPodReady(p) {
			s.logger.WithField("duration", time.Since(start).Milliseconds()).Info("Pod ready already!")
			return podToLocation(p), nil
		}
		if !isPodFinished(p) {
			s.logger.Info("Starting pod found, waiting...")
			wp, err := s.WaitReady(timeout)
			if err != nil {
				return nil, errors.Wrap(err, "Failed to wait for pod")
			}
			s.pod = wp
			s.logger.WithField("duration", time.Since(start).Milliseconds()).Info("Pod ready at last!")
			return podToLocation(wp), nil
		}
	}
	if wasLocked {
		return nil, errors.Errorf("Failed to allocate existent pod")
	}
	annotations := map[string]string{
		"job-id":     s.id,
		"job-type":   s.cfg.Name,
		"info-hash":  s.params.InfoHash,
		"file-path":  s.params.Path,
		"source-url": s.params.SourceURL,
		"extra":      s.params.Extra,
		"grace":      fmt.Sprintf("%d", s.cfg.Grace),
	}
	validLabelValue := regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)
	labels := map[string]string{}
	for k, v := range annotations {
		if validLabelValue.MatchString(v) && len(v) < 64 {
			labels[k] = v
		}
	}
	jobName := s.id + "-" + randStr(4)
	meta := metav1.ObjectMeta{
		Name:        jobName,
		Labels:      labels,
		Annotations: annotations,
	}
	env := []corev1.EnvVar{}
	for k, v := range annotations {
		envName := strings.Replace(strings.ToUpper(k), "-", "_", -1)
		env = append(env, corev1.EnvVar{
			Name:  envName,
			Value: v,
		})
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%v", jobName),
	})
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create watcher")
	}
	defer watcher.Stop()
	watchSuccess := make(chan *corev1.Pod)
	go func() {
		for event := range watcher.ResultChan() {
			pod := event.Object.(*corev1.Pod)
			s.logger.Info(pod.Status.Phase)
			s.pod = pod

			if isPodReady(pod) {
				watchSuccess <- pod
			}
		}
	}()
	ttl := int32(600)
	addStart := time.Now()

	_, err = cl.BatchV1().Jobs(s.namespace).Create(&batchv1.Job{
		ObjectMeta: meta,
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32),
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: meta,
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: s.makeAffinity(),
						},
					},
					Containers: []corev1.Container{
						{
							Name:            s.cfg.Name,
							Image:           s.cfg.Image,
							Env:             env,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources:       s.makeResources(),
							Ports: []corev1.ContainerPort{
								{
									Name:          "grpc",
									ContainerPort: int32(PORT_GRPC),
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "http",
									ContainerPort: int32(PORT_HTTP),
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "probe",
									ContainerPort: int32(PORT_PROBE),
									Protocol:      corev1.ProtocolTCP,
								},
							},
							LivenessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Port: intstr.IntOrString{
											Type:   intstr.String,
											StrVal: "probe",
										},
										Path: POD_LIVENESS_PATH,
									},
								},
							},
							ReadinessProbe: &corev1.Probe{
								PeriodSeconds: 1,
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Port: intstr.IntOrString{
											Type:   intstr.String,
											StrVal: "probe",
										},
										Path: POD_READINESS_PATH,
									},
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	})
	s.logger.WithField("duration", time.Since(addStart).Milliseconds()).Info("Job added")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create job")
	}

	select {
	case <-timeout:
		_ = cl.BatchV1().Jobs(s.namespace).Delete(jobName, &metav1.DeleteOptions{})
		if s.pod != nil {
			_ = cl.CoreV1().Pods(s.namespace).Delete(s.pod.Name, &metav1.DeleteOptions{})
		}
		s.logger.WithField("duration", time.Since(start).Milliseconds()).Error("Failed to initialize job by timeout")
		return nil, errors.Errorf("Got timeout")
	case pod := <-watchSuccess:
		s.pod = pod
		s.logger.WithField("duration", time.Since(start).Milliseconds()).Info("Pod ready!")
		return podToLocation(pod), nil
	}
}

func (s *JobLocation) Get(purge bool) (*Location, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if purge {
		s.inited = false
	}
	if s.inited {
		return s.loc, s.err
	}
	s.loc, s.err = s.get()
	if s.err != nil {
		s.logger.WithError(s.err).Info("Failed to get job location")
	} else {
		s.logger.WithField("location", s.loc).Info("Got job location")
	}
	s.inited = true
	return s.loc, s.err
}
