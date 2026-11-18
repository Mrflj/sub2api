package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestLoadInflightReservationWaitConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int
		waiting int
		wantErr string
	}{
		{"defaults", 30, 8, ""},
		{"disabled", 0, 0, ""},
		{"custom", 60, 4, ""},
		{"negative_timeout", -1, 8, "non-negative"},
		{"negative_waiters", 30, -1, "non-negative"},
		{"excessive_timeout", 301, 8, "must not exceed 300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("BILLING_INFLIGHT_RESERVATION_WAIT_TIMEOUT_SECONDS", "")
			t.Setenv("BILLING_INFLIGHT_RESERVATION_WAIT_MAX_WAITING", "")
			if tc.name != "defaults" {
				viper.Set("billing.inflight_reservation.wait_timeout_seconds", tc.timeout)
				viper.Set("billing.inflight_reservation.wait_max_waiting", tc.waiting)
			}
			cfg, err := Load()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.timeout, cfg.Billing.InflightReservation.WaitTimeoutSeconds)
			require.Equal(t, tc.waiting, cfg.Billing.InflightReservation.WaitMaxWaiting)
		})
	}
}

func TestLoadInflightReservationEstimateConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting *bool
		env     string
		want    bool
	}{
		{name: "default_off"},
		{name: "explicit_opt_in", setting: new(true), want: true},
		{name: "explicit_opt_out", setting: new(false)},
		{name: "environment_opt_in", env: "true", want: true},
		{name: "environment_opt_out", env: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("BILLING_INFLIGHT_RESERVATION_RESERVE_ESTIMATES", tc.env)
			// Existing deployments that only enabled the master switch do not
			// implicitly opt back into estimated-cost admission.
			viper.Set("billing.inflight_reservation.enabled", true)
			if tc.setting != nil {
				viper.Set("billing.inflight_reservation.reserve_estimates", *tc.setting)
			}
			cfg, err := Load()
			require.NoError(t, err)
			require.True(t, cfg.Billing.InflightReservation.Enabled)
			require.Equal(t, tc.want, cfg.Billing.InflightReservation.ReserveEstimates)
		})
	}
}

func TestLoadInflightReservationEstimateConfigFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		env  string
		want bool
	}{
		{name: "existing_config", yaml: "billing:\n  inflight_reservation:\n    enabled: true\n"},
		{name: "file_opt_in", yaml: "billing:\n  inflight_reservation:\n    enabled: true\n    reserve_estimates: true\n", want: true},
		{name: "environment_overrides_file_opt_in", yaml: "billing:\n  inflight_reservation:\n    enabled: true\n    reserve_estimates: true\n", env: "false"},
		{name: "environment_overrides_file_opt_out", yaml: "billing:\n  inflight_reservation:\n    enabled: true\n    reserve_estimates: false\n", env: "true", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("BILLING_INFLIGHT_RESERVATION_RESERVE_ESTIMATES", tc.env)
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o600))
			t.Setenv("CONFIG_FILE", path)
			cfg, err := Load()
			require.NoError(t, err)
			require.True(t, cfg.Billing.InflightReservation.Enabled)
			require.Equal(t, tc.want, cfg.Billing.InflightReservation.ReserveEstimates)
		})
	}
}
