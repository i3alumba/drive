package config

import "testing"

func TestUploadByteDefaultsAndOverrides(t *testing.T) {
	for _, key := range []string{"UPLOAD_MAX_FILE_BYTES", "UPLOAD_STAGING_BUDGET_BYTES", "UPLOAD_FREE_RESERVE_BYTES"} {
		t.Setenv(key, "")
	}
	cfg := Load()
	if cfg.UploadMaxFileBytes != 10<<30 || cfg.UploadBudgetBytes != 20<<30 || cfg.UploadReserveBytes != 2<<30 {
		t.Fatal("unexpected upload limits")
	}
	t.Setenv("UPLOAD_MAX_FILE_BYTES", "1073741824")
	t.Setenv("UPLOAD_STAGING_BUDGET_BYTES", "2147483648")
	t.Setenv("UPLOAD_FREE_RESERVE_BYTES", "0")
	cfg = Load()
	if cfg.UploadMaxFileBytes != 1<<30 || cfg.UploadBudgetBytes != 2<<30 || cfg.UploadReserveBytes != 0 {
		t.Fatal("explicit upload limits ignored")
	}
}

func TestInvalidUploadLimitsNeverSilentlyFallBack(t *testing.T) {
	for _, value := range []string{"0", "-1", "invalid", "999999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("UPLOAD_MAX_FILE_BYTES", value)
			if Load().UploadMaxFileBytes != -1 {
				t.Fatal("invalid safety limit silently replaced by default")
			}
		})
	}
}
