package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadWorkerSettings(t *testing.T) {
	defaults := WorkerSettings{
		AppEnv: "development", HealthAddr: "127.0.0.1:8081",
		Concurrency: 2, JobTimeout: 30 * time.Minute, DrainTimeout: 30 * time.Second,
	}
	tests := []struct {
		name string
		env  map[string]string
		want WorkerSettings
	}{
		{name: "defaults", want: defaults},
		{
			name: "explicit limits",
			env: map[string]string{
				"APP_ENV": "test", "WORKER_ADDR": "[::1]:9001", "WORKER_CONCURRENCY": "4",
				"WORKER_JOB_TIMEOUT": "45m", "WORKER_DRAIN_TIMEOUT": "1m",
			},
			want: WorkerSettings{AppEnv: "test", HealthAddr: "[::1]:9001", Concurrency: 4, JobTimeout: 45 * time.Minute, DrainTimeout: time.Minute},
		},
		{
			name: "production runtime",
			env:  map[string]string{"APP_ENV": "production", "WORKER_ADDR": ":8081"},
			want: WorkerSettings{AppEnv: "production", HealthAddr: ":8081", Concurrency: 2, JobTimeout: 30 * time.Minute, DrainTimeout: 30 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadWorkerSettings(func(key string) (string, bool) { v, ok := tt.env[key]; return v, ok })
			if err != nil || got != tt.want {
				t.Fatalf("loadWorkerSettings() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestLoadWorkerSettingsRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		field string
	}{
		{"unknown environment", map[string]string{"APP_ENV": "staging"}, "APP_ENV"},
		{"empty environment", map[string]string{"APP_ENV": ""}, "APP_ENV"},
		{"empty address", map[string]string{"WORKER_ADDR": ""}, "WORKER_ADDR"},
		{"address missing port", map[string]string{"WORKER_ADDR": "localhost"}, "WORKER_ADDR"},
		{"address URL", map[string]string{"WORKER_ADDR": "http://localhost:8081"}, "WORKER_ADDR"},
		{"address zero port", map[string]string{"WORKER_ADDR": "localhost:0"}, "WORKER_ADDR"},
		{"concurrency zero", map[string]string{"WORKER_CONCURRENCY": "0"}, "WORKER_CONCURRENCY"},
		{"concurrency negative", map[string]string{"WORKER_CONCURRENCY": "-1"}, "WORKER_CONCURRENCY"},
		{"concurrency unbounded", map[string]string{"WORKER_CONCURRENCY": "65"}, "WORKER_CONCURRENCY"},
		{"concurrency text", map[string]string{"WORKER_CONCURRENCY": "SENSITIVE"}, "WORKER_CONCURRENCY"},
		{"empty job timeout", map[string]string{"WORKER_JOB_TIMEOUT": ""}, "WORKER_JOB_TIMEOUT"},
		{"job timeout below minimum", map[string]string{"WORKER_JOB_TIMEOUT": "500ms"}, "WORKER_JOB_TIMEOUT"},
		{"job timeout unbounded", map[string]string{"WORKER_JOB_TIMEOUT": "25h"}, "WORKER_JOB_TIMEOUT"},
		{"job timeout text", map[string]string{"WORKER_JOB_TIMEOUT": "SENSITIVE"}, "WORKER_JOB_TIMEOUT"},
		{"empty drain timeout", map[string]string{"WORKER_DRAIN_TIMEOUT": ""}, "WORKER_DRAIN_TIMEOUT"},
		{"drain timeout below minimum", map[string]string{"WORKER_DRAIN_TIMEOUT": "0s"}, "WORKER_DRAIN_TIMEOUT"},
		{"drain timeout unbounded", map[string]string{"WORKER_DRAIN_TIMEOUT": "6m"}, "WORKER_DRAIN_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings, err := loadWorkerSettings(func(key string) (string, bool) { v, ok := tt.env[key]; return v, ok })
			if err == nil || settings != (WorkerSettings{}) {
				t.Fatalf("invalid config accepted: %+v, %v", settings, err)
			}
			if !strings.Contains(err.Error(), tt.field) || strings.Contains(err.Error(), "SENSITIVE") {
				t.Fatalf("error must identify field without disclosing its value: %v", err)
			}
		})
	}
}

func TestLoadWorkerSettingsDoesNotNeedHTTPConfig(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("HTTP_ADDR", "INVALID_HTTP_ADDR")
	t.Setenv("APP_ORIGIN", "INVALID_HTTP_ORIGIN")
	t.Setenv("SPA_DIR", "")
	if _, err := LoadWorkerSettings(); err != nil {
		t.Fatal("worker must load independently of HTTP configuration")
	}
}
