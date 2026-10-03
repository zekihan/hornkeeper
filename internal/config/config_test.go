package config

import "testing"

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
		{"invalid target", func(c *Config) { c.BackupTarget = "bad_target" }},
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
