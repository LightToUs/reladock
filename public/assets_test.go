package assets

import "testing"

func TestEmbeddedAdminModulesAreAvailable(t *testing.T) {
	for _, name := range []string{"admin.js", "node-bootstrap.js", "node-contact-status.js", "operations-tools.js"} {
		if _, err := Files.ReadFile(name); err != nil {
			t.Fatalf("embedded admin module %q is unavailable: %v", name, err)
		}
	}
}
