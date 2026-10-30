package dataplane

import "testing"

func TestDevelopmentBypassIsExplicitAndPaidValidationStillFailsClosed(t *testing.T) {
	cfg := fixture()
	if err := ValidatePaidConfig(cfg); err == nil {
		t.Fatal("unpaid production configuration was accepted")
	}
	cfg.Rules[0].DevelopmentBypass = true
	if err := ValidatePaidConfig(cfg); err != nil {
		t.Fatalf("explicit development bypass rejected: %v", err)
	}
}
