package config

import (
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
}

func Default() Config {
	return Config{
		LonghornNamespace: "longhorn",
		BackupTarget:      "default",
		Replicas:          3,
		ResyncInterval:    time.Minute,
		LeaderElection:    true,
		LeaderNamespace:   "hornkeeper",
		MetricsAddress:    ":8080",
		HealthAddress:     ":8081",
	}
}

func (c Config) Validate() error {
	if errs := validation.IsDNS1123Label(c.LonghornNamespace); len(errs) != 0 {
		return fmt.Errorf("invalid Longhorn namespace: %s", strings.Join(errs, "; "))
	}
	if err := ValidateBackupTarget(c.BackupTarget); err != nil {
		return fmt.Errorf("invalid default backup target: %w", err)
	}
	if err := ValidateReplicas(c.Replicas); err != nil {
		return fmt.Errorf("invalid default replicas: %w", err)
	}
	if c.ResyncInterval <= 0 {
		return fmt.Errorf("resync interval must be positive")
	}
	if c.LeaderElection {
		if errs := validation.IsDNS1123Label(c.LeaderNamespace); len(errs) != 0 {
			return fmt.Errorf("invalid leader election namespace: %s", strings.Join(errs, "; "))
		}
	}
	return nil
}

func ValidateBackupTarget(name string) error {
	if errs := validation.IsDNS1123Subdomain(name); len(errs) != 0 {
		return fmt.Errorf("backup target must be a nonempty Kubernetes resource name: %s", strings.Join(errs, "; "))
	}
	return nil
}

func ValidateReplicas(count int) error {
	if count < 1 || count > 20 {
		return fmt.Errorf("replicas must be between 1 and 20, got %d", count)
	}
	return nil
}
