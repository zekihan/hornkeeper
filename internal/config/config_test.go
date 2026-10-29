package config

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"empty namespace", func(c *Config) { c.LonghornNamespace = "" }},
		{"invalid namespace", func(c *Config) { c.LonghornNamespace = "Not-valid" }},
		{"empty target", func(c *Config) { c.BackupTarget = "" }},
		{"invalid target", func(c *Config) { c.BackupTarget = "with space" }},
		{"zero replicas", func(c *Config) { c.Replicas = 0 }},
		{"excess replicas", func(c *Config) { c.Replicas = 21 }},
		{"zero interval", func(c *Config) { c.ResyncInterval = 0 }},
		{"negative interval", func(c *Config) { c.ResyncInterval = -1 }},
		{"empty lease namespace", func(c *Config) { c.LeaderNamespace = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	c := Default()
	c.LeaderElection = false
	c.LeaderNamespace = ""
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 20} {
		c.Replicas = n
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupTargetMustNameKubernetesResource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"default", true},
		{"backup-01", true},
		{"backup.example", true},
		{strings.Repeat("a", 63), true},
		{"Backup", false},
		{"backup_target", false},
		{"backup..target", false},
		{"backup.-target", false},
		{"backup-.target", false},
		{strings.Repeat("a", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.BackupTarget = tc.name
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, valid = %v", err, tc.valid)
			}
			if err := ValidateBackupTarget(tc.name); (err == nil) != tc.valid {
				t.Fatalf("ValidateBackupTarget() = %v, valid = %v", err, tc.valid)
			}
		})
	}
}
