package assets

import (
	"bytes"
	"testing"
)

func TestEmbeddedAdminModulesAreAvailable(t *testing.T) {
	for _, name := range []string{"admin.js", "node-bootstrap.js", "node-contact-status.js", "operations-tools.js"} {
		if _, err := Files.ReadFile(name); err != nil {
			t.Fatalf("embedded admin module %q is unavailable: %v", name, err)
		}
	}
}

func TestNodeStatusModuleIncludesIndependentPresentations(t *testing.T) {
	raw, err := Files.ReadFile("node-contact-status.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range [][]byte{[]byte("agentContactPresentation"), []byte("certificatePresentation"), []byte("configurationPresentation"), []byte("probePresentation")} {
		if !bytes.Contains(raw, name) {
			t.Fatalf("status presentation %q is missing", name)
		}
	}
}
