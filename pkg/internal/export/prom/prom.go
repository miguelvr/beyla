package prom

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/mariomac/pipes/pipe"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"

	"github.com/grafana/beyla/pkg/buildinfo"
	"github.com/grafana/beyla/pkg/internal/connector"
	"github.com/grafana/beyla/pkg/internal/export/attributes"
	attr "github.com/grafana/beyla/pkg/internal/export/attributes/names"
	"github.com/grafana/beyla/pkg/internal/export/otel"
	"github.com/grafana/beyla/pkg/internal/pipe/global"
	"github.com/grafana/beyla/pkg/internal/request"
	"github.com/grafana/beyla/pkg/internal/svc"
)

// using labels and names that are equivalent names to the OTEL attributes
// but following the different naming conventions
const (
	SpanMetricsLatency = "traces_spanmetrics_latency"
	SpanMetricsCalls   = "traces_spanmetrics_calls_total"
	SpanMetricsSizes   = "traces_spanmetrics_size_total"
	TracesTargetInfo   = "traces_target_info"

	ServiceGraphClient = "traces_service_graph_request_client_seconds"
	ServiceGraphServer = "traces_service_graph_request_server_seconds"
	ServiceGraphFailed = "traces_service_graph_request_failed_total"
	ServiceGraphTotal  = "traces_service_graph_request_total"

	serviceKey          = "service"
	serviceNamespaceKey = "service_namespace"

	k8sNamespaceName   = "k8s_namespace_name"
	k8sPodName         = "k8s_pod_name"
	k8sDeploymentName  = "k8s_deployment_name"
	k8sStatefulSetName = "k8s_statefulset_name"
	k8sReplicaSetName  = "k8s_replicaset_name"
	k8sDaemonSetName   = "k8s_daemonset_name"
	k8sNodeName        = "k8s_node_name"
	k8sPodUID          = "k8s_pod_uid"
	k8sPodStartTime    = "k8s_pod_start_time"

	spanNameKey          = "span_name"
	statusCodeKey        = "status_code"
	spanKindKey          = "span_kind"
	serviceInstanceKey   = "instance"
	serviceJobKey        = "job"
	sourceKey            = "source"
	telemetryLanguageKey = "telemetry_sdk_language"
	telemetrySDKKey      = "telemetry_sdk_name"

	clientKey          = "client"
	clientNamespaceKey = "client_service_namespace"
	serverKey          = "server"
	serverNamespaceKey = "server_service_namespace"
	connectionTypeKey  = "connection_type"

	// default values for the histogram configuration
	// from https://grafana.com/docs/mimir/latest/send/native-histograms/#migrate-from-classic-histograms
	defaultHistogramBucketFactor     = 1.1
	defaultHistogramMaxBucketNumber  = uint32(100)
	defaultHistogramMinResetDuration = 1 * time.Hour
)

// metrics for Beyla statistics
const (
	BeylaBuildInfo = "beyla_build_info"

	LanguageLabel = "target_lang"
)

// not adding version, as it is a fixed value
var beylaInfoLabelNames = []string{LanguageLabel}

// TODO: TLS
type PrometheusConfig struct {
	Port int    `yaml:"port" env:"BEYLA_PROMETHEUS_PORT"`
	Path string `yaml:"path" env:"BEYLA_PROMETHEUS_PATH"`

	// Deprecated. Going to be removed in Beyla 2.0. Use attributes.select instead
	ReportTarget bool `yaml:"report_target" env:"BEYLA_METRICS_REPORT_TARGET"`
	// Deprecated. Going to be removed in Beyla 2.0. Use attributes.select instead
	ReportPeerInfo bool `yaml:"report_peer" env:"BEYLA_METRICS_REPORT_PEER"`

	DisableBuildInfo bool `yaml:"disable_build_info" env:"BEYLA_PROMETHEUS_DISABLE_BUILD_INFO"`

	// Features of metrics that are can be exported. Accepted values are "application" and "network".
	Features []string `yaml:"features" env:"BEYLA_PROMETHEUS_FEATURES" envSeparator:","`

	Buckets otel.Buckets `yaml:"buckets"`

	// TTL is the time since a metric was updated for the last time until it is
	// removed from the metrics set.
	TTL                         time.Duration `yaml:"ttl" env:"BEYLA_PROMETHEUS_TTL"`
	SpanMetricsServiceCacheSize int           `yaml:"service_cache_size"`

	// Registry is only used for embedding Beyla within the Grafana Agent.
	// It must be nil when Beyla runs as standalone
	Registry *prometheus.Registry `yaml:"-"`
}

func (p PrometheusConfig) SpanMetricsEnabled() bool {
	return slices.Contains(p.Features, otel.FeatureSpan)
}

func (p PrometheusConfig) OTelMetricsEnabled() bool {
	return slices.Contains(p.Features, otel.FeatureApplication)
}

func (p PrometheusConfig) ServiceGraphMetricsEnabled() bool {
	return slices.Contains(p.Features, otel.FeatureGraph)
}

// nolint:gocritic
func (p PrometheusConfig) Enabled() bool {
	return (p.Port != 0 || p.Registry != nil) && (p.OTelMetricsEnabled() || p.SpanMetricsEnabled() || p.ServiceGraphMetricsEnabled())
}

type metricsReporter struct {
	cfg *PrometheusConfig

	beylaInfo             *prometheus.GaugeVec
	httpDuration          *prometheus.HistogramVec
	httpClientDuration    *prometheus.HistogramVec
	grpcDuration          *prometheus.HistogramVec
	grpcClientDuration    *prometheus.HistogramVec
	sqlClientDuration     *prometheus.HistogramVec
	httpRequestSize       *prometheus.HistogramVec
	httpClientRequestSize *prometheus.HistogramVec

	// user-selected attributes for the application-level metrics
	attrHTTPDuration          []attributes.Field[*request.Span, string]
	attrHTTPClientDuration    []attributes.Field[*request.Span, string]
	attrGRPCDuration          []attributes.Field[*request.Span, string]
	attrGRPCClientDuration    []attributes.Field[*request.Span, string]
	attrSQLClientDuration     []attributes.Field[*request.Span, string]
	attrHTTPRequestSize       []attributes.Field[*request.Span, string]
	attrHTTPClientRequestSize []attributes.Field[*request.Span, string]

	// trace span metrics
	spanMetricsLatency    *prometheus.HistogramVec
	spanMetricsCallsTotal *prometheus.CounterVec
	spanMetricsSizeTotal  *prometheus.CounterVec
	tracesTargetInfo      *prometheus.GaugeVec

	// trace service graph
	serviceGraphClient *prometheus.HistogramVec
	serviceGraphServer *prometheus.HistogramVec
	serviceGraphFailed *prometheus.CounterVec
	serviceGraphTotal  *prometheus.CounterVec

	promConnect *connector.PrometheusManager

	bgCtx   context.Context
	ctxInfo *global.ContextInfo

	serviceCache *expirable.LRU[svc.UID, svc.ID]
}

func PrometheusEndpoint(
	ctx context.Context,
	ctxInfo *global.ContextInfo,
	cfg *PrometheusConfig,
	attrSelect attributes.Selection,
) pipe.FinalProvider[[]request.Span] {
	return func() (pipe.FinalFunc[[]request.Span], error) {
		if !cfg.Enabled() {
			return pipe.IgnoreFinal[[]request.Span](), nil
		}
		reporter, err := newReporter(ctx, ctxInfo, cfg, attrSelect)
		if err != nil {
			return nil, fmt.Errorf("instantiating Prometheus endpoint: %w", err)
		}
		if cfg.Registry != nil {
			return reporter.collectMetrics, nil
		}
		return reporter.reportMetrics, nil
	}
}

func newReporter(
	ctx context.Context,
	ctxInfo *global.ContextInfo,
	cfg *PrometheusConfig,
	selector attributes.Selection,
) (*metricsReporter, error) {
	groups := ctxInfo.MetricAttributeGroups
	groups.Add(attributes.GroupPrometheus)

	attrsProvider, err := attributes.NewAttrSelector(groups, selector)
	if err != nil {
		return nil, fmt.Errorf("selecting metrics attributes: %w", err)
	}

	attrHTTPDuration := attributes.PrometheusGetters(request.SpanPromGetters,
		attrsProvider.For(attributes.HTTPServerDuration))
	attrHTTPClientDuration := attributes.PrometheusGetters(request.SpanPromGetters,
		attrsProvider.For(attributes.HTTPClientDuration))
	attrHTTPRequestSize := attributes.PrometheusGetters(request.SpanPromGetters,
		attrsProvider.For(attributes.HTTPServerRequestSize))
	attrHTTPClientRequestSize := attributes.PrometheusGetters(request.SpanPromGetters,
		attrsProvider.For(attributes.HTTPClientRequestSize))
	attrGRPCDuration := attributes.PrometheusGetters(request.SpanPromGetters,
		attrsProvider.For(attributes.RPCServerDuration))
	attrGRPCClientDuration := attributes.PrometheusGetters(request.SpanPromGetters,
		attrsProvider.For(attributes.RPCClientDuration))
	attrSQLClientDuration := attributes.PrometheusGetters(request.SpanPromGetters,
		attrsProvider.For(attributes.HTTPServerDuration))

	// If service name is not explicitly set, we take the service name as set by the
	// executable inspector
	mr := &metricsReporter{
		bgCtx:                     ctx,
		ctxInfo:                   ctxInfo,
		cfg:                       cfg,
		promConnect:               ctxInfo.Prometheus,
		attrHTTPDuration:          attrHTTPDuration,
		attrHTTPClientDuration:    attrHTTPClientDuration,
		attrGRPCDuration:          attrGRPCDuration,
		attrGRPCClientDuration:    attrGRPCClientDuration,
		attrSQLClientDuration:     attrSQLClientDuration,
		attrHTTPRequestSize:       attrHTTPRequestSize,
		attrHTTPClientRequestSize: attrHTTPClientRequestSize,
		beylaInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: BeylaBuildInfo,
			Help: "A metric with a constant '1' value labeled by version, revision, branch, " +
				"goversion from which Beyla was built, the goos and goarch for the build, and the" +
				"language of the reported services",
			ConstLabels: map[string]string{
				"goarch":    runtime.GOARCH,
				"goos":      runtime.GOOS,
				"goversion": runtime.Version(),
				"version":   buildinfo.Version,
				"revision":  buildinfo.Revision,
			},
		}, beylaInfoLabelNames),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            attributes.HTTPServerDuration.Prom,
			Help:                            "duration of HTTP service calls from the server side, in seconds",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNames(attrHTTPDuration)),
		httpClientDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            attributes.HTTPClientDuration.Prom,
			Help:                            "duration of HTTP service calls from the client side, in seconds",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNames(attrHTTPClientDuration)),
		grpcDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            attributes.RPCServerDuration.Prom,
			Help:                            "duration of RCP service calls from the server side, in seconds",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNames(attrGRPCDuration)),
		grpcClientDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            attributes.RPCClientDuration.Prom,
			Help:                            "duration of GRPC service calls from the client side, in seconds",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNames(attrGRPCClientDuration)),
		sqlClientDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            attributes.SQLClientDuration.Prom,
			Help:                            "duration of SQL client operations, in seconds",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNames(attrSQLClientDuration)),
		httpRequestSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            attributes.HTTPServerRequestSize.Prom,
			Help:                            "size, in bytes, of the HTTP request body as received at the server side",
			Buckets:                         cfg.Buckets.RequestSizeHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNames(attrHTTPRequestSize)),
		httpClientRequestSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            attributes.HTTPClientRequestSize.Prom,
			Help:                            "size, in bytes, of the HTTP request body as sent from the client side",
			Buckets:                         cfg.Buckets.RequestSizeHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNames(attrHTTPClientRequestSize)),
		spanMetricsLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            SpanMetricsLatency,
			Help:                            "duration of service calls (client and server), in seconds, in trace span metrics format",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNamesSpans()),
		spanMetricsCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: SpanMetricsCalls,
			Help: "number of service calls in trace span metrics format",
		}, labelNamesSpans()),
		spanMetricsSizeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: SpanMetricsSizes,
			Help: "size of service calls, in bytes, in trace span metrics format",
		}, labelNamesSpans()),
		tracesTargetInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: TracesTargetInfo,
			Help: "target service information in trace span metric format",
		}, labelNamesTargetInfo(ctxInfo)),
		serviceGraphClient: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            ServiceGraphClient,
			Help:                            "duration of client service calls, in seconds, in trace service graph metrics format",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNamesServiceGraph()),
		serviceGraphServer: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                            ServiceGraphServer,
			Help:                            "duration of server service calls, in seconds, in trace service graph metrics format",
			Buckets:                         cfg.Buckets.DurationHistogram,
			NativeHistogramBucketFactor:     defaultHistogramBucketFactor,
			NativeHistogramMaxBucketNumber:  defaultHistogramMaxBucketNumber,
			NativeHistogramMinResetDuration: defaultHistogramMinResetDuration,
		}, labelNamesServiceGraph()),
		serviceGraphFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: ServiceGraphFailed,
			Help: "number of failed service calls in trace service graph metrics format",
		}, labelNamesServiceGraph()),
		serviceGraphTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: ServiceGraphTotal,
			Help: "number of service calls in trace service graph metrics format",
		}, labelNamesServiceGraph()),
	}

	if cfg.SpanMetricsEnabled() {
		mr.serviceCache = expirable.NewLRU(cfg.SpanMetricsServiceCacheSize, func(_ svc.UID, v svc.ID) {
			lv := mr.labelValuesTargetInfo(v)
			mr.tracesTargetInfo.WithLabelValues(lv...).Sub(1)
		}, cfg.TTL)
	}

	var registeredMetrics []prometheus.Collector
	if !mr.cfg.DisableBuildInfo {
		registeredMetrics = append(registeredMetrics, mr.beylaInfo)
	}

	if cfg.OTelMetricsEnabled() {
		registeredMetrics = append(registeredMetrics,
			mr.httpClientRequestSize,
			mr.httpClientDuration,
			mr.grpcClientDuration,
			mr.sqlClientDuration,
			mr.httpRequestSize,
			mr.httpDuration,
			mr.grpcDuration)
	}

	if cfg.SpanMetricsEnabled() {
		registeredMetrics = append(registeredMetrics,
			mr.spanMetricsLatency,
			mr.spanMetricsCallsTotal,
			mr.spanMetricsSizeTotal,
			mr.tracesTargetInfo,
		)
	}

	if cfg.ServiceGraphMetricsEnabled() {
		registeredMetrics = append(registeredMetrics,
			mr.serviceGraphClient,
			mr.serviceGraphServer,
			mr.serviceGraphFailed,
			mr.serviceGraphTotal,
		)
	}

	if mr.cfg.Registry != nil {
		mr.cfg.Registry.MustRegister(registeredMetrics...)
	} else {
		mr.promConnect.Register(cfg.Port, cfg.Path, registeredMetrics...)
	}

	return mr, nil
}

func (r *metricsReporter) reportMetrics(input <-chan []request.Span) {
	go r.promConnect.StartHTTP(r.bgCtx)
	r.collectMetrics(input)
}

func (r *metricsReporter) collectMetrics(input <-chan []request.Span) {
	for spans := range input {
		for i := range spans {
			r.observe(&spans[i])
		}
	}
}

func (r *metricsReporter) observe(span *request.Span) {
	t := span.Timings()
	r.beylaInfo.WithLabelValues(span.ServiceID.SDKLanguage.String()).Set(1.0)
	duration := t.End.Sub(t.RequestStart).Seconds()
	if r.cfg.OTelMetricsEnabled() {
		switch span.Type {
		case request.EventTypeHTTP:
			r.httpDuration.WithLabelValues(
				labelValues(span, r.attrHTTPDuration)...,
			).Observe(duration)
			r.httpRequestSize.WithLabelValues(
				labelValues(span, r.attrHTTPRequestSize)...,
			).Observe(float64(span.ContentLength))
		case request.EventTypeHTTPClient:
			r.httpClientDuration.WithLabelValues(
				labelValues(span, r.attrHTTPClientDuration)...,
			).Observe(duration)
			r.httpClientRequestSize.WithLabelValues(
				labelValues(span, r.attrHTTPClientRequestSize)...,
			).Observe(float64(span.ContentLength))
		case request.EventTypeGRPC:
			r.grpcDuration.WithLabelValues(
				labelValues(span, r.attrGRPCDuration)...,
			).Observe(duration)
		case request.EventTypeGRPCClient:
			r.grpcClientDuration.WithLabelValues(
				labelValues(span, r.attrGRPCClientDuration)...,
			).Observe(duration)
		case request.EventTypeSQLClient:
			r.sqlClientDuration.WithLabelValues(
				labelValues(span, r.attrSQLClientDuration)...,
			).Observe(duration)
		}
	}
	if r.cfg.SpanMetricsEnabled() {
		lv := r.labelValuesSpans(span)
		r.spanMetricsLatency.WithLabelValues(lv...).Observe(duration)
		r.spanMetricsCallsTotal.WithLabelValues(lv...).Add(1)
		r.spanMetricsSizeTotal.WithLabelValues(lv...).Add(float64(span.ContentLength))

		_, ok := r.serviceCache.Get(span.ServiceID.UID)
		if !ok {
			r.serviceCache.Add(span.ServiceID.UID, span.ServiceID)
			lv = r.labelValuesTargetInfo(span.ServiceID)
			r.tracesTargetInfo.WithLabelValues(lv...).Add(1)
		}
	}

	if r.cfg.ServiceGraphMetricsEnabled() {
		lvg := r.labelValuesServiceGraph(span)
		if span.IsClientSpan() {
			r.serviceGraphClient.WithLabelValues(lvg...).Observe(duration)
		} else {
			r.serviceGraphServer.WithLabelValues(lvg...).Observe(duration)
		}
		r.serviceGraphTotal.WithLabelValues(lvg...).Add(1)
		if otel.SpanStatusCode(span) == codes.Error {
			r.serviceGraphFailed.WithLabelValues(lvg...).Add(1)
		}
	}
}

func appendK8sLabelNames(names []string) []string {
	names = append(names, k8sNamespaceName, k8sPodName, k8sNodeName, k8sPodUID, k8sPodStartTime,
		k8sDeploymentName, k8sReplicaSetName, k8sStatefulSetName, k8sDaemonSetName)
	return names
}

func appendK8sLabelValuesService(values []string, service svc.ID) []string {
	// must follow the order in appendK8sLabelNames
	values = append(values,
		service.Metadata[(attr.K8sNamespaceName)],
		service.Metadata[(attr.K8sPodName)],
		service.Metadata[(attr.K8sNodeName)],
		service.Metadata[(attr.K8sPodUID)],
		service.Metadata[(attr.K8sPodStartTime)],
		service.Metadata[(attr.K8sDeploymentName)],
		service.Metadata[(attr.K8sReplicaSetName)],
		service.Metadata[(attr.K8sStatefulSetName)],
		service.Metadata[(attr.K8sDaemonSetName)],
	)
	return values
}

func labelNamesSpans() []string {
	return []string{serviceKey, serviceNamespaceKey, spanNameKey, statusCodeKey, spanKindKey, serviceInstanceKey, serviceJobKey, sourceKey}
}

func (r *metricsReporter) labelValuesSpans(span *request.Span) []string {
	job := span.ServiceID.Name
	if span.ServiceID.Namespace != "" {
		job = span.ServiceID.Namespace + "/" + job
	}
	return []string{
		span.ServiceID.Name,
		span.ServiceID.Namespace,
		otel.TraceName(span),
		strconv.Itoa(int(otel.SpanStatusCode(span))),
		otel.SpanKindString(span),
		span.ServiceID.Instance,
		job,
		"beyla",
	}
}

func labelNamesTargetInfo(ctxInfo *global.ContextInfo) []string {
	names := []string{serviceKey, serviceNamespaceKey, serviceInstanceKey, serviceJobKey, telemetryLanguageKey, telemetrySDKKey, sourceKey}

	if ctxInfo.K8sEnabled {
		names = appendK8sLabelNames(names)
	}

	return names
}

func (r *metricsReporter) labelValuesTargetInfo(service svc.ID) []string {
	job := service.Name
	if service.Namespace != "" {
		job = service.Namespace + "/" + job
	}
	values := []string{
		service.Name,
		service.Namespace,
		service.Instance,
		job,
		service.SDKLanguage.String(),
		"beyla",
		"beyla",
	}

	if r.ctxInfo.K8sEnabled {
		values = appendK8sLabelValuesService(values, service)
	}

	return values
}

func labelNamesServiceGraph() []string {
	return []string{clientKey, clientNamespaceKey, serverKey, serverNamespaceKey, connectionTypeKey, sourceKey}
}

func (r *metricsReporter) labelValuesServiceGraph(span *request.Span) []string {
	if span.IsClientSpan() {
		return []string{
			request.SpanPeer(span),
			span.ServiceID.Namespace,
			request.SpanHost(span),
			span.OtherNamespace,
			"virtual_node",
			"beyla",
		}
	}
	return []string{
		request.SpanPeer(span),
		span.OtherNamespace,
		request.SpanHost(span),
		span.ServiceID.Namespace,
		"virtual_node",
		"beyla",
	}
}

func labelNames(getters []attributes.Field[*request.Span, string]) []string {
	labels := make([]string, 0, len(getters))
	for _, label := range getters {
		labels = append(labels, label.ExposedName)
	}
	return labels
}

func labelValues(s *request.Span, getters []attributes.Field[*request.Span, string]) []string {
	values := make([]string, 0, len(getters))
	for _, getter := range getters {
		values = append(values, getter.Get(s))
	}
	return values
}
