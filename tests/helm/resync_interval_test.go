package helm_test

import (
	"bytes"
	"os/exec"
	"testing"
	"time"

	"github.com/zekihan/hornkeeper/internal/config"
)

func TestChartResyncInterval(t *testing.T) {
	helm := helmBinary(t)
	for _, tc := range []struct {
		name, interval string
		valid          bool
	}{
		{name: "sub-nanosecond", interval: "0.1ns"},
		{name: "components each round to zero", interval: "0.5ns0.5ns"},
		{name: "overflow", interval: "9223372036854775808ns"},
		{name: "one nanosecond", interval: "1ns", valid: true},
		{name: "mixed units", interval: "1m0.5s", valid: true},
		{name: "maximum duration", interval: "9223372036854775807ns", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			interval, startupErr := time.ParseDuration(tc.interval)
			if startupErr == nil {
				cfg.ResyncInterval = interval
				startupErr = cfg.Validate()
			}
			if (startupErr == nil) != tc.valid {
				t.Fatalf("controller validation = %v, valid = %v", startupErr, tc.valid)
			}

			output, err := exec.CommandContext(t.Context(), helm, "template", "hornkeeper", "../../charts/hornkeeper", "--set-string", "controller.resyncInterval="+tc.interval).
				CombinedOutput()
			if tc.valid {
				if err != nil ||
					!bytes.Contains(output, []byte(`"--resync-interval=`+tc.interval+`"`)) {
					t.Fatalf(
						"expected Helm to pass through interval %q, got %v\n%s",
						tc.interval,
						err,
						output,
					)
				}
				return
			}
			if err == nil {
				t.Fatalf(
					"Helm accepted interval %q, which fails controller startup: %v",
					tc.interval,
					startupErr,
				)
			}
			if !bytes.Contains(output, []byte("resyncInterval")) {
				t.Fatalf("expected an error identifying controller.resyncInterval, got %s", output)
			}
		})
	}
}
