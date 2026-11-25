package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

type Config struct {
	LonghornNamespace string
	BackupTarget      string
	Replicas          int
	ResyncInterval    time.Duration
	LeaderElection    bool
	LeaderNamespace   string
	MetricsAddress    string
	HealthAddress     string
	DryRun            bool
}

const defaultReplicas = 3

func Default() Config {
	return Config{
		LonghornNamespace: "longhorn",
		BackupTarget:      "default",
		Replicas:          defaultReplicas,
		ResyncInterval:    time.Minute,
		LeaderElection:    true,
		LeaderNamespace:   "hornkeeper",
		MetricsAddress:    "0",
		HealthAddress:     ":8081",
	}
}

func (cfg *Config) Validate() error {
	if errs := validation.IsDNS1123Label(cfg.LonghornNamespace); len(errs) != 0 {
		return fmt.Errorf("invalid Longhorn namespace: %s", strings.Join(errs, "; "))
	}
	if err := ValidateBackupTarget(cfg.BackupTarget); err != nil {
		return fmt.Errorf("invalid default backup target: %w", err)
	}
	if err := ValidateReplicas(cfg.Replicas); err != nil {
		return fmt.Errorf("invalid default replicas: %w", err)
	}
	if cfg.ResyncInterval <= 0 {
		return errors.New("resync interval must be positive")
	}
	if cfg.LeaderElection {
		if errs := validation.IsDNS1123Label(cfg.LeaderNamespace); len(errs) != 0 {
			return fmt.Errorf("invalid leader election namespace: %s", strings.Join(errs, "; "))
		}
	}
	return nil
}

func ValidateBackupTarget(name string) error {
	if name == "" {
		return errors.New("backup target must not be empty")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) != 0 {
		return fmt.Errorf(
			"backup target must be a valid Kubernetes resource name: %s",
			strings.Join(errs, "; "),
		)
	}
	return nil
}

func ValidateReplicas(count int) error {
	if count < 1 || count > 20 {
		return fmt.Errorf("replicas must be between 1 and 20, got %d", count)
	}
	return nil
}
