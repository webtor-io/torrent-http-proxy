package services

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bsm/redislock"
	"github.com/prometheus/client_golang/prometheus"
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
	POD_LOCK_DURATION       = 1
	POD_LOCK_STANDBY        = 1
	POD_LIVENESS_PATH       = "/liveness"
	POD_READINESS_PATH      = "/readiness"
	JOB_NODE_AFFINITY_KEY   = "job-node-affinity-key"
	JOB_NODE_AFFINITY_VALUE = "job-node-affinity-value"
	JOB_NAMESPACE           = "job-namespace"
	JOB_REQUEST_AFFINITY    = "job-request-affinity"
	MY_NODE_NAME            = "my-node-name"
)

var (
	promJobInvokeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "webtor_job_invoke_duration_seconds",
		Help:    "Job invoke duration in seconds",
		Buckets: prometheus.LinearBuckets(2.5, 2.5, 20),
	}, []string{"name"})
	promJobInvokeCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "webtor_job_invoke_current",
		Help: "Job invoke current",
	}, []string{"name"})
	promJobInvokeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_job_invoke_total",
		Help: "Job invoke total",
	}, []string{"name"})
	promJobInvokeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_job_invoke_errors",
		Help: "Job invoke errors",
	}, []string{"name"})
)

func init() {
	prometheus.MustRegister(promJobInvokeDuration)
	prometheus.MustRegister(promJobInvokeCurrent)
	prometheus.MustRegister(promJobInvokeTotal)
	prometheus.MustRegister(promJobInvokeErrors)
}

type JobLocation struct {
	id             string
	cl             *K8SClient
	loc            *Location
	finish         chan error
	cfg            *JobConfig
	pod            *corev1.Pod
	params         *InitParams
	inited         bool
	err            error
	mux            sync.Mutex
	logger         *logrus.Entry
	l              *Locker
	naKey          string
	naVal          string
	namespace      string
	extAddressType string
	acl            *Client
	ra             bool
	nn             string
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
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   JOB_REQUEST_AFFINITY,
		Usage:  "Job request affinity",
		EnvVar: "JOB_REQUEST_AFFINITY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   MY_NODE_NAME,
		Usage:  "My node name",
		Value:  "",
		EnvVar: "MY_NODE_NAME",
	})
}

func NewJobLocation(c *cli.Context, cfg *JobConfig, params *InitParams, cl *K8SClient, logger *logrus.Entry, l *Locker, acl *Client) *JobLocation {
	id := MakeJobID(cfg, params)
	return &JobLocation{cfg: cfg, params: params, cl: cl, id: id, inited: false, acl: acl,
		logger: logger, l: l, naKey: c.String(JOB_NODE_AFFINITY_KEY), naVal: c.String(JOB_NODE_AFFINITY_VALUE),
		namespace: c.String(JOB_NAMESPACE), extAddressType: c.String(WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE),
		ra: c.Bool(JOB_REQUEST_AFFINITY), nn: c.String(MY_NODE_NAME),
	}
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

func (s *JobLocation) podToLocation(pod *corev1.Pod) (*Location, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get K8S client")
	}
	extIP := ""
	nodeName := pod.Spec.NodeName
	nodes, err := cl.CoreV1().Nodes().List(metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", nodeName).String(),
	})
	if err != nil {
		s.logger.WithError(err).Errorf("Failed to get node with name=%s", nodeName)
	}
	if len(nodes.Items) == 0 {
		s.logger.Warnf("No nodes found by name=%s", nodeName)
	}
	if err == nil && len(nodes.Items) > 0 {
		for _, a := range nodes.Items[0].Status.Addresses {
			if a.Type == corev1.NodeAddressType(s.extAddressType) {
				extIP = a.Address
			}
		}
	}

	w, err := s.waitFinish(pod)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to init expire channel")
	}
	return &Location{
		IP: net.ParseIP(pod.Status.PodIP),
		Ports: Ports{
			HTTP:  PORT_HTTP,
			GRPC:  PORT_GRPC,
			Probe: PORT_PROBE,
		},
		HostIP:      net.ParseIP(extIP),
		Unavailable: false,
		Expire:      w,
	}, nil
}

func (s *JobLocation) waitFinish(pod *corev1.Pod) (chan bool, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get K8S client")
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", pod.Name).String(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create watcher")
	}
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
			url := fmt.Sprintf("http://%s:%d%s", pod.Status.PodIP, PORT_PROBE, POD_LIVENESS_PATH)
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
	resCh := make(chan bool)
	go func() {
		select {
		case <-watchSuccess:
			s.logger.Info("Pod finished")
		case <-httpHealthCheck:
			s.logger.Info("Job health check failed")
		}
		watcher.Stop()
		close(resCh)
	}()
	return resCh, nil
}

func (s *JobLocation) waitReady(pod *corev1.Pod, ctx context.Context) (*corev1.Pod, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get K8S client")
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", pod.Name).String(),
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
				s.logger.Info("Pod ready!")
				watchSuccess <- pod
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
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
			corev1.ResourceCPU: resource.MustParse(s.cfg.CPULimits),
		}
	}
	return res

}

func (s *JobLocation) makeNodeSelector() map[string]string {
	res := map[string]string{}
	if s.naKey != "" && s.naVal != "" {
		res[s.naKey] = s.naVal
	}
	return res
}

func (s *JobLocation) makeRequiredNodeAffinity() *corev1.NodeSelector {
	nst := []corev1.NodeSelectorTerm{}
	nst = append(nst, corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{
				Key:      fmt.Sprintf("%vno-job", K8S_LABEL_PREFIX),
				Operator: corev1.NodeSelectorOpNotIn,
				Values:   []string{"true"},
			},
		},
	})
	nst = append(nst, corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{
				Key:      fmt.Sprintf("%vno-%v", K8S_LABEL_PREFIX, s.cfg.Name),
				Operator: corev1.NodeSelectorOpNotIn,
				Values:   []string{"true"},
			},
		},
	})
	return &corev1.NodeSelector{NodeSelectorTerms: nst}
}

func (s *JobLocation) makeNodeAffinity() []corev1.PreferredSchedulingTerm {
	aff := []corev1.PreferredSchedulingTerm{}
	if s.ra && s.nn != "" {
		aff = append(aff, corev1.PreferredSchedulingTerm{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{
						Key:      "kubernetes.io/hostname",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{s.nn},
					},
				},
			},
		})
	}
	return aff
}

func (s *JobLocation) isInited() (bool, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return false, errors.Wrap(err, "Failed to get K8S client")
	}
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%vjob-id=%v", K8S_LABEL_PREFIX, s.id),
	}
	pods, err := cl.CoreV1().Pods(s.namespace).List(opts)
	if err != nil {
		return false, errors.Wrap(err, "Failed to find active job")
	}
	for _, p := range pods.Items {
		if !isPodFinished(&p) {
			return true, nil
		}
	}
	return false, nil
}

func (s *JobLocation) waitForPod(ctx context.Context, name string) (*corev1.Pod, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get K8S client")
	}

	selector := fmt.Sprintf("%vjob-id=%v", K8S_LABEL_PREFIX, s.id)
	if name != "" {
		selector = fmt.Sprintf("job-name=%v", K8S_LABEL_PREFIX, name)
	}
	opts := metav1.ListOptions{
		LabelSelector: selector,
	}
	pods, err := cl.CoreV1().Pods(s.namespace).List(opts)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to find active job")
	}
	for _, p := range pods.Items {
		if isPodReady(&p) {
			s.logger.Info("Pod ready already!")
			return &p, nil
		}
	}
	for _, p := range pods.Items {
		if !isPodFinished(&p) {
			s.logger.Info("Starting pod found, waiting...")
			wp, err := s.waitReady(&p, ctx)
			if err != nil {
				return nil, errors.Wrap(err, "Failed to wait for pod")
			}
			s.logger.Info("Pod ready at last!")
			return wp, nil
		}
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(opts)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create watcher")
	}
	defer watcher.Stop()
	watchSuccess := make(chan *corev1.Pod)
	go func() {
		for event := range watcher.ResultChan() {
			pod := event.Object.(*corev1.Pod)
			s.logger.Info(pod.Status.Phase)
			if isPodReady(pod) {
				watchSuccess <- pod
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case pod := <-watchSuccess:
		return pod, nil
	}
}

func (s *JobLocation) invoke() (*Location, error) {
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
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Minute)
	isInited, err := s.isInited()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to check is there any inited job")
	}
	if isInited {
		pod, err := s.waitForPod(ctx, "")
		if err != nil {
			return nil, errors.Wrap(err, "Failed to wait for pod")
		}
		loc, err := s.podToLocation(pod)

		if err != nil {
			return nil, errors.Wrap(err, "Failed to convert pod to location")
		}
		return loc, nil
	}
	if wasLocked {
		return nil, errors.Errorf("Failed to allocate existent pod")
	}
	clientName := "default"
	if s.acl != nil {
		clientName = s.acl.Name
	}
	jobName := s.id + "-" + randStr(4)
	annotations := map[string]string{
		"job-name":    jobName,
		"job-id":      s.id,
		"job-type":    s.cfg.Name,
		"info-hash":   s.params.InfoHash,
		"file-path":   s.params.Path,
		"origin-path": s.params.OriginPath,
		"source-url":  s.params.SourceURL,
		"extra":       s.params.Extra,
		"grace":       fmt.Sprintf("%d", s.cfg.Grace),
		"client":      clientName,
	}
	annotationsWithPrefix := map[string]string{}
	for k, v := range annotations {
		annotationsWithPrefix[fmt.Sprintf("%v%v", K8S_LABEL_PREFIX, k)] = v
	}
	validLabelValue := regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)
	labels := map[string]string{}
	for k, v := range annotationsWithPrefix {
		if validLabelValue.MatchString(v) && len(v) < 64 {
			labels[k] = v
		}
	}
	meta := metav1.ObjectMeta{
		Name:        jobName,
		Labels:      labels,
		Annotations: annotationsWithPrefix,
	}
	env := []corev1.EnvVar{
		{
			Name:  "USE_SNAPSHOT",
			Value: s.cfg.UseSnapshot,
		},
		{

			Name:  "SNAPSHOT_START_THRESHOLD",
			Value: fmt.Sprintf("%f", s.cfg.SnapshotStartThreshold),
		},
		{

			Name:  "SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD",
			Value: fmt.Sprintf("%f", s.cfg.SnapshotStartFullDownloadThreshold),
		},
		{
			Name:  "SNAPSHOT_DOWNLOAD_RATIO",
			Value: fmt.Sprintf("%f", s.cfg.SnapshotDownloadRatio),
		},
		{
			Name:  "SNAPSHOT_TORRENT_SIZE_LIMIT",
			Value: fmt.Sprintf("%d", s.cfg.SnapshotTorrentSizeLimit),
		},
		{
			Name:  "AWS_ACCESS_KEY_ID",
			Value: s.cfg.AWSAccessKeyID,
		},
		{
			Name:  "AWS_SECRET_ACCESS_KEY",
			Value: s.cfg.AWSSecretAccessKey,
		},
		{
			Name:  "AWS_REGION",
			Value: s.cfg.AWSRegion,
		},
		{
			Name:  "AWS_BUCKET",
			Value: s.cfg.AWSBucket,
		},
		{
			Name:  "AWS_BUCKET_SPREAD",
			Value: s.cfg.AWSBucketSpread,
		},
		{
			Name:  "AWS_NO_SSL",
			Value: s.cfg.AWSNoSSL,
		},
		{
			Name:  "AWS_ENDPOINT",
			Value: s.cfg.AWSEndpoint,
		},
	}
	for k, v := range annotations {
		envName := strings.Replace(strings.ToUpper(k), "-", "_", -1)
		env = append(env, corev1.EnvVar{
			Name:  envName,
			Value: v,
		})
	}

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
					NodeSelector: s.makeNodeSelector(),
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 50,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												fmt.Sprintf("%vinfo-hash", K8S_LABEL_PREFIX): s.params.InfoHash,
											},
										},
										TopologyKey: "kubernetes.io/hostname",
									},
								},
							},
						},
						NodeAffinity: &corev1.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: s.makeNodeAffinity(),
							RequiredDuringSchedulingIgnoredDuringExecution:  s.makeRequiredNodeAffinity(),
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

	pod, err := s.waitForPod(ctx, jobName)

	if err != nil {
		_ = cl.BatchV1().Jobs(s.namespace).Delete(jobName, &metav1.DeleteOptions{})
		s.logger.WithError(err).WithField("duration", time.Since(start).Milliseconds()).Error("Failed to initialize job")
		return nil, errors.Wrap(err, "Failed to initialize job")
	}

	loc, err := s.podToLocation(pod)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to convert pod to location")
	}
	return loc, nil

}

func (s *JobLocation) wait() (*Location, error) {
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Minute)

	pod, err := s.waitForPod(ctx, "")

	if err != nil {
		return nil, errors.Wrap(err, "Failed to initialize job")
	}

	loc, err := s.podToLocation(pod)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to convert pod to location")
	}
	return loc, nil
}

func (s *JobLocation) Wait() (*Location, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.loc, s.err
	}
	s.loc, s.err = s.wait()
	if s.err != nil {
		s.logger.WithError(s.err).Info("Failed to get job location")
	} else {
		s.logger.WithField("location", s.loc.IP).Info("Got job location")
	}
	s.inited = true
	return s.loc, s.err
}

func (s *JobLocation) Get() (*Location, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.loc, s.err
	}
	return nil, errors.Errorf("JobLocation not inited yet")
}

func (s *JobLocation) Invoke(purge bool) (*Location, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if purge {
		s.inited = false
	}
	if s.inited {
		return s.loc, s.err
	}
	now := time.Now()
	promJobInvokeCurrent.WithLabelValues(s.cfg.Name).Inc()
	promJobInvokeTotal.WithLabelValues(s.cfg.Name).Inc()
	s.loc, s.err = s.invoke()
	promJobInvokeCurrent.WithLabelValues(s.cfg.Name).Dec()
	promJobInvokeDuration.WithLabelValues(s.cfg.Name).Observe(time.Since(now).Seconds())
	if s.err != nil {
		s.logger.WithError(s.err).Info("Failed to get job location")
		promJobInvokeErrors.WithLabelValues(s.cfg.Name).Inc()
	} else {
		s.logger.WithField("location", s.loc.IP).Info("Got job location")
	}
	s.inited = true
	return s.loc, s.err
}
