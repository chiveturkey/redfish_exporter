package collector

import (
	"bytes"
	"fmt"
	"sync"
	"time"
	"strings"

	"github.com/apex/log"
	"github.com/prometheus/client_golang/prometheus"
	gofish "github.com/stmcginnis/gofish"
	gofishcommon "github.com/stmcginnis/gofish/common"
	redfish "github.com/stmcginnis/gofish/redfish"
)

// Metric name parts.
const (
	// Exporter namespace.
	namespace = "redfish"
	// Subsystem(s).
	exporter = "exporter"
	// Math constant for picoseconds to seconds.
	picoSeconds = 1e12
)

// sync.Map of RedfishClients by hostname/IP.
type RedfishClients struct {
	sync.Map
}

// Metric descriptors.
var (
	totalScrapeDurationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "collector_duration_seconds"),
		"Collector time duration.",
		nil, nil,
	)
	redfishClients RedfishClients
)

// RedfishCollector collects redfish metrics. It implements prometheus.Collector.
type RedfishCollector struct {
	redfishClient *gofish.APIClient
	collectors    map[string]prometheus.Collector
	redfishUp     prometheus.Gauge
}

// NewRedfishCollector return RedfishCollector
func NewRedfishCollector(host string, username string, password string, logger *log.Entry) *RedfishCollector {
	var collectors map[string]prometheus.Collector
	collectorLogCtx := logger
	redfishClient, err := newRedfishClient(host, username, password, collectorLogCtx)
	if err != nil {
		collectorLogCtx.WithError(err).Error("error creating redfish client")
	} else {
		// Add client to redfishClients sync.Map.
		redfishClients.Store(host, redfishClient)

		chassisCollector := NewChassisCollector(namespace, redfishClient, collectorLogCtx)
		systemCollector := NewSystemCollector(namespace, redfishClient, collectorLogCtx)
		managerCollector := NewManagerCollector(namespace, redfishClient, collectorLogCtx)

		collectors = map[string]prometheus.Collector{"chassis": chassisCollector, "system": systemCollector, "manager": managerCollector}
	}
	collectorLogCtx.WithField("clients", redfishClients).Debug("stored redfish clients")

	return &RedfishCollector{
		redfishClient: redfishClient,
		collectors:    collectors,
		redfishUp: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: "",
				Name:      "up",
				Help:      "redfish up",
			},
		),
	}
}

// Describe implements prometheus.Collector.
func (r *RedfishCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, collector := range r.collectors {
		collector.Describe(ch)
	}

}

// Collect implements prometheus.Collector.
func (r *RedfishCollector) Collect(ch chan<- prometheus.Metric) {

	scrapeTime := time.Now()
	if r.redfishClient != nil {
		// TODO: Make the following conditional on a new 'use_sessions' (or similar) config var.
		// defer r.redfishClient.Logout()
		r.redfishUp.Set(1)
		wg := &sync.WaitGroup{}
		wg.Add(len(r.collectors))

		for _, collector := range r.collectors {
			go func(collector prometheus.Collector) {
				defer wg.Done()
				collector.Collect(ch)
			}(collector)
		}
		wg.Wait()
	} else {
		r.redfishUp.Set(0)
	}

	ch <- r.redfishUp
	ch <- prometheus.MustNewConstMetric(totalScrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds())
}

// clientConnected determines whether a stored Redfish Client Session is still active by querying
// active Sessions on the host and checking that the Client Session ID is present in the host
// Sessions list.
func clientConnected(redfishClient *gofish.APIClient, logger *log.Entry) (bool) {
	collectorLogCtx := logger

	var sessionPresent bool = false
	clientSession, err := redfishClient.GetSession()
	if err != nil {
		panic(err)
	}
	// Remove trailing '/' if present.
	clientSessionIDURL := strings.TrimSuffix(clientSession.ID, "/")
	// Example clientSessionIDURL: /redfish/v1/SessionService/Sessions/1234
	clientSessionIDURLElements := strings.Split(clientSessionIDURL, "/")
	clientSessionID := clientSessionIDURLElements[len(clientSessionIDURLElements) - 1]

	service := redfishClient.Service
	sessions, err := service.Sessions()
	if err != nil {
		collectorLogCtx.WithField("operation", "service.Sessions()").WithError(err).Error("error getting session list from system")
		// We may be unable to query some sessions (especially if they are ephemeral).  Our goal is to
		// prove connectivity.  As long as we can query at least one session, then we are able to
		// connect.  Return false if we are unable to connect to any sessions.
		if len(sessions) == 0 {
			return false
		}
	}
	for _, session := range sessions {
		if session.ID == clientSessionID {
			sessionPresent = true
		}
	}

	return sessionPresent
}

func createClient(host string, username string, password string) (*gofish.APIClient, error) {
	url := fmt.Sprintf("https://%s", host)

	config := gofish.ClientConfig{
		Endpoint: url,
		Username: username,
		Password: password,
		Insecure: true,
	}
	redfishClient, err := gofish.Connect(config)
	if err != nil {
		return nil, err
	}
	return redfishClient, nil
}

func newRedfishClient(host string, username string, password string, logger *log.Entry) (*gofish.APIClient, error) {
	client_result, target_found := redfishClients.Load(host)
	if target_found {
		existing_client := client_result.(*gofish.APIClient)
		if clientConnected(existing_client, logger) {
			return existing_client, nil
		} else {
			return createClient(host, username, password)
		}
	} else {
		return createClient(host, username, password)
	}
}

func parseCommonStatusHealth(status gofishcommon.Health) (float64, bool) {
	if bytes.Equal([]byte(status), []byte("OK")) {
		return float64(1), true
	} else if bytes.Equal([]byte(status), []byte("Warning")) {
		return float64(2), true
	} else if bytes.Equal([]byte(status), []byte("Critical")) {
		return float64(3), true
	}
	return float64(0), false
}

func parseCommonStatusState(status gofishcommon.State) (float64, bool) {

	if bytes.Equal([]byte(status), []byte("")) {
		return float64(0), false
	} else if bytes.Equal([]byte(status), []byte("Enabled")) {
		return float64(1), true
	} else if bytes.Equal([]byte(status), []byte("Disabled")) {
		return float64(2), true
	} else if bytes.Equal([]byte(status), []byte("StandbyOffinline")) {
		return float64(3), true
	} else if bytes.Equal([]byte(status), []byte("StandbySpare")) {
		return float64(4), true
	} else if bytes.Equal([]byte(status), []byte("InTest")) {
		return float64(5), true
	} else if bytes.Equal([]byte(status), []byte("Starting")) {
		return float64(6), true
	} else if bytes.Equal([]byte(status), []byte("Absent")) {
		return float64(7), true
	} else if bytes.Equal([]byte(status), []byte("UnavailableOffline")) {
		return float64(8), true
	} else if bytes.Equal([]byte(status), []byte("Deferring")) {
		return float64(9), true
	} else if bytes.Equal([]byte(status), []byte("Quiesced")) {
		return float64(10), true
	} else if bytes.Equal([]byte(status), []byte("Updating")) {
		return float64(11), true
	}
	return float64(0), false
}

func parseCommonPowerState(status redfish.PowerState) (float64, bool) {
	if bytes.Equal([]byte(status), []byte("On")) {
		return float64(1), true
	} else if bytes.Equal([]byte(status), []byte("Off")) {
		return float64(2), true
	} else if bytes.Equal([]byte(status), []byte("PoweringOn")) {
		return float64(3), true
	} else if bytes.Equal([]byte(status), []byte("PoweringOff")) {
		return float64(4), true
	}
	return float64(0), false
}

func parseLinkStatus(status redfish.LinkStatus) (float64, bool) {
	if bytes.Equal([]byte(status), []byte("LinkUp")) {
		return float64(1), true
	} else if bytes.Equal([]byte(status), []byte("NoLink")) {
		return float64(2), true
	} else if bytes.Equal([]byte(status), []byte("LinkDown")) {
		return float64(3), true
	}
	return float64(0), false
}

func parsePortLinkStatus(status redfish.PortLinkStatus) (float64, bool) {
	if bytes.Equal([]byte(status), []byte("Up")) {
		return float64(1), true
	} 
	return float64(0), false
}
func boolToFloat64(data bool) float64 {

	if data {
		return float64(1)
	}
	return float64(0)

}

func parsePhySecReArmMethod(method redfish.IntrusionSensorReArm) (float64, bool) {
	if bytes.Equal([]byte(method), []byte("Manual")) {
		return float64(1), true
	}
	if bytes.Equal([]byte(method), []byte("Automatic")) {
		return float64(2), true
	}

	return float64(0), false
}

func parsePhySecIntrusionSensor(method redfish.IntrusionSensor) (float64, bool) {
	if bytes.Equal([]byte(method), []byte("Normal")) {
		return float64(1), true
	}
	if bytes.Equal([]byte(method), []byte("TamperingDetected")) {
		return float64(2), true
	}
	if bytes.Equal([]byte(method), []byte("HardwareIntrusion")) {
		return float64(3), true
	}

	return float64(0), false
}
