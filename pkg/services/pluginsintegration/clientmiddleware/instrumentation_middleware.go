package clientmiddleware

import (
	"context"
	"errors"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/tracing"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/plugins/manager/registry"
)

// pluginMetrics contains the prometheus metrics used by the InstrumentationMiddleware.
type pluginMetrics struct {
	pluginRequestCounter         *prometheus.CounterVec
	pluginRequestDuration        *prometheus.HistogramVec
	pluginRequestSize            *prometheus.HistogramVec
	pluginRequestDurationSeconds *prometheus.HistogramVec
}

// InstrumentationMiddleware is a middleware that instruments plugin requests.
// It tracks requests count, duration and size as prometheus metrics.
// It also enriches the [context.Context] with a contextual logger containing plugin and request details.
// For those reasons, this middleware should live at the top of the middleware stack.
type InstrumentationMiddleware struct {
	pluginMetrics
	pluginRegistry registry.Service
	next           plugins.Client
}

func newInstrumentationMiddleware(promRegisterer prometheus.Registerer, pluginRegistry registry.Service) *InstrumentationMiddleware {
	pluginRequestCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grafana",
		Name:      "plugin_request_total",
		Help:      "The total amount of plugin requests",
	}, []string{"plugin_id", "endpoint", "status", "target"})
	pluginRequestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "grafana",
		Name:      "plugin_request_duration_milliseconds",
		Help:      "Plugin request duration",
		Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 25, 50, 100},
	}, []string{"plugin_id", "endpoint", "target"})
	pluginRequestSize := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "grafana",
			Name:      "plugin_request_size_bytes",
			Help:      "histogram of plugin request sizes returned",
			Buckets:   []float64{128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576},
		}, []string{"source", "plugin_id", "endpoint", "target"},
	)
	pluginRequestDurationSeconds := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "grafana",
		Name:      "plugin_request_duration_seconds",
		Help:      "Plugin request duration in seconds",
		Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 25},
	}, []string{"source", "plugin_id", "endpoint", "status", "target"})
	promRegisterer.MustRegister(
		pluginRequestCounter,
		pluginRequestDuration,
		pluginRequestSize,
		pluginRequestDurationSeconds,
	)
	return &InstrumentationMiddleware{
		pluginMetrics: pluginMetrics{
			pluginRequestCounter:         pluginRequestCounter,
			pluginRequestDuration:        pluginRequestDuration,
			pluginRequestSize:            pluginRequestSize,
			pluginRequestDurationSeconds: pluginRequestDurationSeconds,
		},
		pluginRegistry: pluginRegistry,
	}
}

// NewInstrumentationMiddleware returns a new InstrumentationMiddleware.
func NewInstrumentationMiddleware(promRegisterer prometheus.Registerer, pluginRegistry registry.Service) plugins.ClientMiddleware {
	imw := newInstrumentationMiddleware(promRegisterer, pluginRegistry)
	return plugins.ClientMiddlewareFunc(func(next plugins.Client) plugins.Client {
		imw.next = next
		return imw
	})
}

// pluginTarget returns the value for the "target" Prometheus label for the given plugin ID.
func (m *InstrumentationMiddleware) pluginTarget(ctx context.Context, pluginID string) (string, error) {
	p, exists := m.pluginRegistry.Plugin(ctx, pluginID)
	if !exists {
		return "", plugins.ErrPluginNotRegistered
	}
	return string(p.Target()), nil
}

// instrumentContext adds a contextual logger with plugin and request details to the given context.
func instrumentContext(ctx context.Context, endpoint string, pCtx backend.PluginContext) context.Context {
	p := []any{"endpoint", endpoint, "pluginId", pCtx.PluginID}
	if pCtx.DataSourceInstanceSettings != nil {
		p = append(p, "dsName", pCtx.DataSourceInstanceSettings.Name)
		p = append(p, "dsUID", pCtx.DataSourceInstanceSettings.UID)
	}
	if pCtx.User != nil {
		p = append(p, "uname", pCtx.User.Login)
	}
	return log.WithContextualAttributes(ctx, p)
}

// instrumentPluginRequestSize tracks the size of the given request in the m.pluginRequestSize metric.
func (m *InstrumentationMiddleware) instrumentPluginRequestSize(ctx context.Context, pluginCtx backend.PluginContext, endpoint string, requestSize float64) error {
	target, err := m.pluginTarget(ctx, pluginCtx.PluginID)
	if err != nil {
		return err
	}
	m.pluginRequestSize.WithLabelValues("grafana-backend", pluginCtx.PluginID, endpoint, target).Observe(requestSize)
	return nil
}

// instrumentPluginRequest increments the m.pluginRequestCounter metric and tracks the duration of the given request.
func (m *InstrumentationMiddleware) instrumentPluginRequest(ctx context.Context, pluginCtx backend.PluginContext, endpoint string, fn func(context.Context) error) error {
	target, err := m.pluginTarget(ctx, pluginCtx.PluginID)
	if err != nil {
		return err
	}

	status := statusOK
	start := time.Now()

	ctx = instrumentContext(ctx, endpoint, pluginCtx)
	err = fn(ctx)
	if err != nil {
		status = statusError
		if errors.Is(err, context.Canceled) {
			status = statusCancelled
		}
	}

	elapsed := time.Since(start)

	pluginRequestDurationWithLabels := m.pluginRequestDuration.WithLabelValues(pluginCtx.PluginID, endpoint, target)
	pluginRequestCounterWithLabels := m.pluginRequestCounter.WithLabelValues(pluginCtx.PluginID, endpoint, status, target)
	pluginRequestDurationSecondsWithLabels := m.pluginRequestDurationSeconds.WithLabelValues("grafana-backend", pluginCtx.PluginID, endpoint, status, target)

	if traceID := tracing.TraceIDFromContext(ctx, true); traceID != "" {
		pluginRequestDurationWithLabels.(prometheus.ExemplarObserver).ObserveWithExemplar(
			float64(elapsed/time.Millisecond), prometheus.Labels{"traceID": traceID},
		)
		pluginRequestCounterWithLabels.(prometheus.ExemplarAdder).AddWithExemplar(1, prometheus.Labels{"traceID": traceID})
		pluginRequestDurationSecondsWithLabels.(prometheus.ExemplarObserver).ObserveWithExemplar(
			elapsed.Seconds(), prometheus.Labels{"traceID": traceID},
		)
	} else {
		pluginRequestDurationWithLabels.Observe(float64(elapsed / time.Millisecond))
		pluginRequestCounterWithLabels.Inc()
		pluginRequestDurationSecondsWithLabels.Observe(elapsed.Seconds())
	}

	return err
}

func (m *InstrumentationMiddleware) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	var requestSize float64
	for _, v := range req.Queries {
		requestSize += float64(len(v.JSON))
	}
	if err := m.instrumentPluginRequestSize(ctx, req.PluginContext, endpointQueryData, requestSize); err != nil {
		return nil, err
	}
	var resp *backend.QueryDataResponse
	err := m.instrumentPluginRequest(ctx, req.PluginContext, endpointQueryData, func(ctx context.Context) (innerErr error) {
		resp, innerErr = m.next.QueryData(ctx, req)
		return innerErr
	})
	return resp, err
}

func (m *InstrumentationMiddleware) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	if err := m.instrumentPluginRequestSize(ctx, req.PluginContext, endpointCallResource, float64(len(req.Body))); err != nil {
		return err
	}
	return m.instrumentPluginRequest(ctx, req.PluginContext, endpointCallResource, func(ctx context.Context) error {
		return m.next.CallResource(ctx, req, sender)
	})
}

func (m *InstrumentationMiddleware) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	var result *backend.CheckHealthResult
	err := m.instrumentPluginRequest(ctx, req.PluginContext, endpointCheckHealth, func(ctx context.Context) (innerErr error) {
		result, innerErr = m.next.CheckHealth(ctx, req)
		return
	})
	return result, err
}

func (m *InstrumentationMiddleware) CollectMetrics(ctx context.Context, req *backend.CollectMetricsRequest) (*backend.CollectMetricsResult, error) {
	var result *backend.CollectMetricsResult
	err := m.instrumentPluginRequest(ctx, req.PluginContext, endpointCollectMetrics, func(ctx context.Context) (innerErr error) {
		result, innerErr = m.next.CollectMetrics(ctx, req)
		return
	})
	return result, err
}

func (m *InstrumentationMiddleware) SubscribeStream(ctx context.Context, req *backend.SubscribeStreamRequest) (*backend.SubscribeStreamResponse, error) {
	return m.next.SubscribeStream(ctx, req)
}

func (m *InstrumentationMiddleware) PublishStream(ctx context.Context, req *backend.PublishStreamRequest) (*backend.PublishStreamResponse, error) {
	return m.next.PublishStream(ctx, req)
}

func (m *InstrumentationMiddleware) RunStream(ctx context.Context, req *backend.RunStreamRequest, sender *backend.StreamSender) error {
	return m.next.RunStream(ctx, req, sender)
}
