package config

import (
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
