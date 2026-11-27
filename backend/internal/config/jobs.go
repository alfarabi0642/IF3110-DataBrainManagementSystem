package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultWorkerConcurrency  = 2
	defaultWorkerJobTimeout   = 30 * time.Minute
	defaultWorkerDrainTimeout = 30 * time.Second
	maxWorkerConcurrency      = 64
	maxWorkerJobTimeout       = 24 * time.Hour
	maxWorkerDrainTimeout     = 5 * time.Minute
)

// WorkerSettings adalah batas runtime worker antrean. Loader ini tidak membaca konfigurasi HTTP,
// cookie, atau SPA karena worker berjalan sebagai proses terpisah.
type WorkerSettings struct {
	AppEnv       string
	HealthAddr   string
	Concurrency  int
	JobTimeout   time.Duration
	DrainTimeout time.Duration
}

// LoadWorkerSettings membaca environment worker. Nilai di luar batas ditolak sebelum worker membuka
// koneksi atau menyentuh database; pesan error tidak memuat nilainya.
func LoadWorkerSettings() (WorkerSettings, error) {
	return loadWorkerSettings(os.LookupEnv)
}

func loadWorkerSettings(lookup func(string) (string, bool)) (WorkerSettings, error) {
	value := func(name, fallback string) string {
		if v, ok := lookup(name); ok {
			return v
		}
		return fallback
	}
	settings := WorkerSettings{
		AppEnv:     value("APP_ENV", "development"),
		HealthAddr: value("WORKER_ADDR", "127.0.0.1:8081"),
	}
	switch settings.AppEnv {
	case "development", "test", "production":
	default:
		return WorkerSettings{}, fmt.Errorf("APP_ENV must be development, test, or production")
	}
	host, port, err := net.SplitHostPort(settings.HealthAddr)
	if err != nil || !validPort(port) || strings.ContainsAny(host, " \t\r\n/@?#\\%") {
		return WorkerSettings{}, fmt.Errorf("WORKER_ADDR must be a host:port address with port 1-65535")
	}
	concurrency, err := strconv.Atoi(value("WORKER_CONCURRENCY", strconv.Itoa(defaultWorkerConcurrency)))
	if err != nil || concurrency < 1 || concurrency > maxWorkerConcurrency {
		return WorkerSettings{}, fmt.Errorf("WORKER_CONCURRENCY must be an integer between 1 and %d", maxWorkerConcurrency)
	}
	settings.Concurrency = concurrency
	var ok bool
	if settings.JobTimeout, ok = workerDuration(value("WORKER_JOB_TIMEOUT", defaultWorkerJobTimeout.String()), time.Second, maxWorkerJobTimeout); !ok {
		return WorkerSettings{}, fmt.Errorf("WORKER_JOB_TIMEOUT must be a duration between 1s and %s", maxWorkerJobTimeout)
	}
	if settings.DrainTimeout, ok = workerDuration(value("WORKER_DRAIN_TIMEOUT", defaultWorkerDrainTimeout.String()), time.Second, maxWorkerDrainTimeout); !ok {
		return WorkerSettings{}, fmt.Errorf("WORKER_DRAIN_TIMEOUT must be a duration between 1s and %s", maxWorkerDrainTimeout)
	}
	return settings, nil
}

func workerDuration(raw string, min, max time.Duration) (time.Duration, bool) {
	value, err := time.ParseDuration(raw)
	return value, err == nil && value >= min && value <= max
}
