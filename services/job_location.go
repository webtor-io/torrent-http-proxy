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
	podHTTPPort              = 8080
	podProbePort             = 8081
	podGRPCPort              = 50051
	podHealthCheckTimeout    = 5
	podHealthCheckInterval   = 5
	podHealthCheckTries      = 3
	podLockDuration          = 30
	podLockStandby           = 1
	podInitInterval          = 3
	podInitTries             = 5
	podLivenessPath          = "/liveness"
	podReadinessPath         = "/readiness"
	jobNodeAffinityKeyFlag   = "job-node-affinity-key"
	jobNodeAffinityValueFlag = "job-node-affinity-value"
	jobNamespaceFlag         = "job-namespace"
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
	nn             string
}

func RegisterJobFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   jobNodeAffinityKeyFlag,
			Usage:  "Node Affinity Key",
			Value:  "",
			EnvVar: "JOB_NODE_AFFINITY_KEY",
		},
		cli.StringFlag{
			Name:   jobNodeAffinityValueFlag,
			Usage:  "Node Affinity Value",
			Value:  "",
			EnvVar: "JOB_NODE_AFFINITY_VALUE",
		},
		cli.StringFlag{
			Name:   jobNamespaceFlag,
			Usage:  "Job namespace",
			Value:  "webtor",
			EnvVar: "JOB_NAMESPACE",
		},
	)
}

func NewJobLocation(c *cli.Context, cfg *JobConfig, params *InitParams, cl *K8SClient, logger *logrus.Entry, l *Locker, acl *Client) *JobLocation {
	id := MakeJobID(cfg, params)
	return &JobLocation{cfg: cfg, params: params, cl: cl, id: id, inited: false, acl: acl,
		logger: logger, l: l,
		naKey:          c.String(jobNodeAffinityKeyFlag),
		naVal:          c.String(jobNodeAffinityValueFlag),
		namespace:      c.String(jobNamespaceFlag),
		extAddressType: c.String(webOriginHostRedirectAddressTypeFlag),
		nn:             c.String(myNodeNameFlag),
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
	w, err := s.waitFinish(pod)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init expire channel")
	}
	return &Location{
		IP: net.ParseIP(pod.Status.PodIP),
		Ports: Ports{
			HTTP:  podHTTPPort,
			GRPC:  podGRPCPort,
			Probe: podProbePort,
		},
		Unavailable: false,
		Expire:      w,
	}, nil
}

func (s *JobLocation) waitFinish(pod *corev1.Pod) (chan bool, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get K8S client")
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", pod.Name).String(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create watcher")
	}
	watchSuccess := make(chan *corev1.Pod)
	go func() {
		for event := range watcher.ResultChan() {
			pod := event.Object.(*corev1.Pod)

			s.logger.Info(pod.Status.Phase)

			if isPodFinished(pod) {
				s.logger.Info("pod finished!")
				watchSuccess <- pod
			}
		}
	}()
	httpHealthCheck := make(chan error)
	finished := false
	go func() {
		netClient := &http.Client{
			Timeout: time.Second * podHealthCheckTimeout,
		}
		tries := 0
		url := fmt.Sprintf("http://%s:%d%s", pod.Status.PodIP, podProbePort, podLivenessPath)
		for {
			if finished {
				return
			}
			// s.logger.Infof("Checking url=%s", url)
			res, err := netClient.Get(url)
			if res != nil && res.StatusCode != http.StatusOK {
				err = errors.Errorf("got not OK status code=%v", res.StatusCode)
			}
			if err != nil {
				tries++
				s.logger.WithError(err).Warn("failed to check pod status")
				if tries >= podHealthCheckTries {
					httpHealthCheck <- err
					return
				}
			} else if res.StatusCode == http.StatusOK {
				tries = 0
			}
			time.Sleep(time.Second * podHealthCheckInterval)
		}
	}()
	resCh := make(chan bool)
	go func() {
		select {
		case <-watchSuccess:
			s.logger.Info("pod finished")
			finished = true
		case err := <-httpHealthCheck:
			s.logger.WithError(err).Warnf("job health check failed for pod=%v", pod.Name)
		}
		watcher.Stop()
		close(resCh)
	}()
	return resCh, nil
}

func (s *JobLocation) waitReady(pod *corev1.Pod, ctx context.Context) (*corev1.Pod, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get K8S client")
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", pod.Name).String(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create watcher")
	}
	defer watcher.Stop()
	watchSuccess := make(chan *corev1.Pod)
	go func() {
		for event := range watcher.ResultChan() {
			pod := event.Object.(*corev1.Pod)

			if isPodReady(pod) {
				s.logger.Info("pod ready!")
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
	if s.cfg.CPURequests != "" || s.cfg.MemoryRequests != "" {
		requests := corev1.ResourceList{}
		if s.cfg.CPURequests != "" {
			requests[corev1.ResourceCPU] = resource.MustParse(s.cfg.CPURequests)
		}
		if s.cfg.MemoryRequests != "" {
			requests[corev1.ResourceMemory] = resource.MustParse(s.cfg.MemoryRequests)
		}
		res.Requests = requests
	}
	if s.cfg.CPULimits != "" || s.cfg.MemoryLimits != "" {
		limits := corev1.ResourceList{}
		if s.cfg.CPULimits != "" {
			limits[corev1.ResourceCPU] = resource.MustParse(s.cfg.CPULimits)
		}
		if s.cfg.MemoryLimits != "" {
			limits[corev1.ResourceMemory] = resource.MustParse(s.cfg.MemoryLimits)
		}
		res.Limits = limits
	}
	return res

}

func (s *JobLocation) makeNodeSelector() map[string]string {
	res := map[string]string{}
	if s.naKey != "" && s.naVal != "" {
		res[s.naKey] = s.naVal
	}
	if s.cfg.AffinityKey != "" && s.cfg.AffinityValue != "" {
		res[s.cfg.AffinityKey] = s.cfg.AffinityValue
	}
	return res
}

func (s *JobLocation) makeRequiredNodeAffinity() *corev1.NodeSelector {
	var nst []corev1.NodeSelectorTerm
	nst = append(nst, corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{
				Key:      fmt.Sprintf("%vno-job", k8SLabelPrefix),
				Operator: corev1.NodeSelectorOpNotIn,
				Values:   []string{"true"},
			},
		},
	})
	nst = append(nst, corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{
				Key:      fmt.Sprintf("%vno-%v", k8SLabelPrefix, s.cfg.Name),
				Operator: corev1.NodeSelectorOpNotIn,
				Values:   []string{"true"},
			},
		},
	})
	return &corev1.NodeSelector{NodeSelectorTerms: nst}
}

func (s *JobLocation) makeNodeAffinity() []corev1.PreferredSchedulingTerm {
	var aff []corev1.PreferredSchedulingTerm
	if s.cfg.RequestAffinity && s.nn != "" {
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
		return false, errors.Wrap(err, "failed to get K8S client")
	}
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%vjob-id=%v", k8SLabelPrefix, s.id),
	}
	pods, err := cl.CoreV1().Pods(s.namespace).List(opts)
	if err != nil {
		return false, errors.Wrap(err, "failed to find active job")
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
		return nil, errors.Wrap(err, "failed to get K8S client")
	}

	selector := fmt.Sprintf("%vjob-id=%v", k8SLabelPrefix, s.id)
	if name != "" {
		selector = fmt.Sprintf("job-name=%v", name)
	}
	opts := metav1.ListOptions{
		LabelSelector: selector,
	}
	pods, err := cl.CoreV1().Pods(s.namespace).List(opts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to find active job")
	}
	for _, p := range pods.Items {
		if isPodReady(&p) {
			s.logger.Info("pod ready already!")
			return &p, nil
		}
	}
	for _, p := range pods.Items {
		if !isPodFinished(&p) {
			s.logger.Info("starting pod found, waiting...")
			wp, err := s.waitReady(&p, ctx)
			if err != nil {
				return nil, errors.Wrap(err, "failed to wait for pod")
			}
			s.logger.Info("pod ready at last!")
			return wp, nil
		}
	}
	watcher, err := cl.CoreV1().Pods(s.namespace).Watch(opts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create watcher")
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
	s.logger.Info("job initialization started")
	start := time.Now()
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get K8S client")
	}
	wasLocked := false
	l, err := s.l.Get().Obtain(s.id, time.Second*podLockDuration, nil)
	if err == redislock.ErrNotObtained {
		s.logger.Warn("failed to obtain lock")
		wasLocked = true
		time.Sleep(time.Second * podLockStandby)
	} else if err != nil {
		return nil, errors.Wrap(err, "failed to set lock")
	} else {
		defer func(l *redislock.Lock) {
			_ = l.Release()
		}(l)
	}

	isInited := false
	for i := 0; i < podInitTries; i++ {
		isInited, err = s.isInited()
		if err != nil {
			return nil, errors.Wrap(err, "failed to check is there any inited job")
		}
		if isInited || !wasLocked {
			break
		}
		time.Sleep(time.Second * podInitInterval)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if isInited {
		pod, err := s.waitForPod(ctx, "")
		if err != nil {
			return nil, errors.Wrap(err, "failed to wait for pod")
		}
		loc, err := s.podToLocation(pod)

		if err != nil {
			return nil, errors.Wrap(err, "failed to convert pod to location")
		}
		return loc, nil
	}
	if wasLocked {
		return nil, errors.Errorf("failed to allocate existent pod")
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
		annotationsWithPrefix[fmt.Sprintf("%v%v", k8SLabelPrefix, k)] = v
	}
	validLabelValue := regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)
	labels := map[string]string{}
	for k, v := range annotationsWithPrefix {
		if validLabelValue.MatchString(v) && len(v) < 64 {
			labels[k] = v
		}
	}
	for k, v := range s.cfg.Labels {
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
			Name:  "TO_COMPLETION",
			Value: fmt.Sprintf("%v", s.cfg.ToCompletion),
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
		{
			Name:  "HTTP_PROXY",
			Value: s.cfg.HTTPProxy,
		},
	}
	if s.cfg.Env != nil {
		for k, v := range s.cfg.Env {
			env = append(env, corev1.EnvVar{
				Name:  k,
				Value: v,
			})
		}
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
												fmt.Sprintf("%vinfo-hash", k8SLabelPrefix): s.params.InfoHash,
											},
										},
										TopologyKey: "kubernetes.io/hostname",
									},
								},
							},
						},
						NodeAffinity: &corev1.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: s.makeNodeAffinity(),
							// RequiredDuringSchedulingIgnoredDuringExecution:  s.makeRequiredNodeAffinity(),
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
									ContainerPort: int32(podGRPCPort),
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "http",
									ContainerPort: int32(podHTTPPort),
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "probe",
									ContainerPort: int32(podProbePort),
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
										Path: podLivenessPath,
									},
								},
								InitialDelaySeconds: int32(10),
								PeriodSeconds:       int32(10),
								FailureThreshold:    int32(10),
								TimeoutSeconds:      int32(10),
							},
							ReadinessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Port: intstr.IntOrString{
											Type:   intstr.String,
											StrVal: "probe",
										},
										Path: podReadinessPath,
									},
								},
								InitialDelaySeconds: int32(0),
								PeriodSeconds:       int32(1),
								FailureThreshold:    int32(10),
								TimeoutSeconds:      int32(1),
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
		return nil, errors.Wrap(err, "failed to create job")
	}

	pod, err := s.waitForPod(ctx, jobName)

	if err != nil {
		_ = cl.BatchV1().Jobs(s.namespace).Delete(jobName, &metav1.DeleteOptions{})
		s.logger.WithError(err).WithField("duration", time.Since(start).Milliseconds()).Error("failed to initialize job")
		return nil, errors.Wrap(err, "failed to initialize job")
	}

	loc, err := s.podToLocation(pod)

	if err != nil {
		return nil, errors.Wrap(err, "failed to convert pod to location")
	}
	return loc, nil

}

func (s *JobLocation) wait() (*Location, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pod, err := s.waitForPod(ctx, "")

	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize job")
	}

	loc, err := s.podToLocation(pod)

	if err != nil {
		return nil, errors.Wrap(err, "failed to convert pod to location")
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
		s.logger.WithError(s.err).Info("failed to get job location")
	} else {
		s.logger.WithField("location", s.loc.IP).Info("got job location")
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
		s.logger.WithError(s.err).Info("failed to get job location")
		promJobInvokeErrors.WithLabelValues(s.cfg.Name).Inc()
	} else {
		s.logger.WithField("location", s.loc.IP).Info("got job location")
	}
	s.inited = true
	return s.loc, s.err
}
