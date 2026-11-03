package config

import "testing"

func TestURLImportLimits(t *testing.T) {
	for _, key := range []string{"URL_IMPORT_MAX_FILE_BYTES", "URL_IMPORT_STAGING_BUDGET_BYTES", "URL_IMPORT_FREE_RESERVE_BYTES", "URL_IMPORT_TIMEOUT_SECONDS"} {
		t.Setenv(key, "")
	}
	cfg := Load()
	if cfg.ImportMaxFileBytes != 10<<30 || cfg.ImportBudgetBytes != 20<<30 || cfg.ImportReserveBytes != 2<<30 || cfg.ImportTimeoutS != 7200 {
		t.Fatal("unexpected URL import defaults")
	}
	t.Setenv("URL_IMPORT_MAX_FILE_BYTES", "5368709120")
	t.Setenv("URL_IMPORT_STAGING_BUDGET_BYTES", "10737418240")
	t.Setenv("URL_IMPORT_FREE_RESERVE_BYTES", "0")
	t.Setenv("URL_IMPORT_TIMEOUT_SECONDS", "10800")
	cfg = Load()
	if cfg.ImportMaxFileBytes != 5<<30 || cfg.ImportBudgetBytes != 10<<30 || cfg.ImportReserveBytes != 0 || cfg.ImportTimeoutS != 10800 {
		t.Fatal("URL import overrides ignored")
	}
}
func TestInvalidURLImportSettingsFailClosed(t *testing.T) {
	for _, value := range []string{"0", "-1", "invalid", "99999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("URL_IMPORT_MAX_FILE_BYTES", value)
			if Load().ImportMaxFileBytes != -1 {
				t.Fatal("invalid limit silently relaxed")
			}
		})
	}
	for _, value := range []string{"0", "-1", "86401", "9223372036854775807", "invalid"} {
		t.Run("timeout_"+value, func(t *testing.T) {
			t.Setenv("URL_IMPORT_TIMEOUT_SECONDS", value)
			if Load().ImportTimeoutS != -1 {
				t.Fatal("invalid deadline accepted")
			}
		})
	}
}
