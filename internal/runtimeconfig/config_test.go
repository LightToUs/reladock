package runtimeconfig

import "testing"

func TestLocalDefaults(t *testing.T) {
	c, err := Load(func(string) string { return "" })
	if err != nil || c.Address != "127.0.0.1:3001" || c.Origin != "http://127.0.0.1:3001" || c.AgentAddress != "127.0.0.1:3002" || c.AllowUnentitledRules {
		t.Fatalf("local defaults: %+v %v", c, err)
	}
}

func TestDevelopmentEntitlementBypassRequiresExactBoolean(t *testing.T) {
	c, err := Load(func(k string) string {
		if k == "RELADOCK_DEVELOPMENT_ALLOW_UNENTITLED_RULES" {
			return "true"
		}
		return ""
	})
	if err != nil || !c.AllowUnentitledRules {
		t.Fatalf("explicit bypass not loaded: %+v %v", c, err)
	}
	if _, err = Load(func(k string) string {
		if k == "RELADOCK_DEVELOPMENT_ALLOW_UNENTITLED_RULES" {
			return "1"
		}
		return ""
	}); err == nil {
		t.Fatal("ambiguous bypass value accepted")
	}
}

func TestProductionRejectsUnsafeConfiguration(t *testing.T) {
	base := map[string]string{"APP_ENV": "production", "PUBLIC_ORIGIN": "https://console.example.com", "TLS_CERT_FILE": "/cert.pem", "TLS_KEY_FILE": "/key.pem", "LISTEN_ADDRESS": "0.0.0.0:8443"}
	for _, tc := range []struct{ key, value string }{
		{"PUBLIC_ORIGIN", ""}, {"PUBLIC_ORIGIN", "http://console.example.com"},
		{"PUBLIC_ORIGIN", "https://user:pass@console.example.com"}, {"PUBLIC_ORIGIN", "https://console.example.com/path"},
		{"PUBLIC_ORIGIN", "https://console.example.com?bad=1"}, {"TLS_KEY_FILE", ""},
		{"TLS_CERT_FILE", ""}, {"LISTEN_ADDRESS", "bad:port"}, {"APP_ENV", "prodution"},
		{"AGENT_LISTEN_ADDRESS", "0.0.0.0:3002"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			values[tc.key] = tc.value
			if _, err := Load(func(k string) string { return values[k] }); err == nil {
				t.Fatal("accepted unsafe config")
			}
		})
	}
	c, err := Load(func(k string) string { return base[k] })
	if err != nil || !c.Production {
		t.Fatalf("valid production: %v", err)
	}
}

func TestPublicBindingAlwaysNeedsTLS(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:3001", "[::]:3001", "192.0.2.1:3001"} {
		if _, err := Load(func(k string) string {
			if k == "LISTEN_ADDRESS" {
				return addr
			}
			return ""
		}); err == nil {
			t.Fatal("public plain HTTP accepted")
		}
	}
}
