package xds

import (
	"strings"
	"sync"
	"time"

	metricsservice "github.com/envoyproxy/go-control-plane/envoy/service/metrics/v3"
	prom "github.com/prometheus/client_model/go"
)

type Scaler interface {
	ScaleToZero(cluster string)
	ScaleFromZero(cluster string)
}

type Stat struct {
	Eps int
	Val float64
	Tms int64
}

const TriggerMetric = "upstream_rq_total"

type MonitorOptions struct {
	ScaleToZeroAfter time.Duration
	ScaleToZeroCheck time.Duration
}

func NewMonitorServer(scaler Scaler, options MonitorOptions) *MonitorServer {
	s := &MonitorServer{
		clusters: make(map[string]Stat),
		scaler:   scaler,
	}
	go func() {
		for {
			time.Sleep(options.ScaleToZeroCheck)
			now := time.Now().UnixNano() / 1e6

			s.mu.Lock()
			for cluster, stat := range s.clusters {
				if stat.Eps > 0 && now-stat.Tms > options.ScaleToZeroAfter.Milliseconds() {
					go scaler.ScaleToZero(cluster)
				}
			}
			s.mu.Unlock()
		}
	}()
	return s
}

type MonitorServer struct {
	mu       sync.Mutex
	clusters map[string]Stat
	scaler   Scaler
}

func (s *MonitorServer) TrackCluster(cluster string, endpoints int) {
	s.mu.Lock()
	if prev, ok := s.clusters[cluster]; ok {
		prev.Eps = endpoints
		s.clusters[cluster] = prev
	} else {
		go s.scaler.ScaleToZero(cluster) // initial scale down
		s.clusters[cluster] = Stat{Eps: endpoints, Tms: time.Now().UnixNano() / 1e6}
	}
	s.mu.Unlock()
}

func (s *MonitorServer) RemoveCluster(cluster string) {
	s.mu.Lock()
	delete(s.clusters, cluster)
	s.mu.Unlock()
}

func (s *MonitorServer) StreamMetrics(server metricsservice.MetricsService_StreamMetricsServer) error {
	defer server.SendAndClose(&metricsservice.StreamMetricsResponse{})
	for {
		msg, err := server.Recv()
		if err != nil {
			return err
		}
		s.mu.Lock()
		for _, m := range msg.EnvoyMetrics {
			if *m.Type == prom.MetricType_COUNTER && len(m.Metric) > 0 {
				s.processCounter(m)
			}
		}
		s.mu.Unlock()
	}
}

func (s *MonitorServer) processCounter(m *prom.MetricFamily) {
	if mn := *m.Name; strings.HasSuffix(mn, TriggerMetric) {
		if parts := strings.Split(mn, "."); len(parts) == 3 {
			name := parts[1]
			prev := s.clusters[name]

			val := *m.Metric[0].Counter.Value
			tms := *m.Metric[0].TimestampMs

			if prev.Val != val {
				prev.Val = val
				prev.Tms = tms
				if val != 0 {
					go s.scaler.ScaleFromZero(name)
				}
			}

			s.clusters[name] = prev
		}
	}
}
