package assets

import (
	"bytes"
	"testing"
)

func TestEmbeddedAdminModulesAreAvailable(t *testing.T) {
	for _, name := range []string{"admin.js", "console-navigation.js", "lightsail-admin.js", "node-action-dialog.js", "node-bootstrap.js", "node-contact-status.js", "node-content-dialog.js", "node-editor.js", "node-visibility.js", "operations-tools.js", "plan-catalog.js", "resource-mode.js", "source-flow-logging.js"} {
		if _, err := Files.ReadFile(name); err != nil {
			t.Fatalf("embedded admin module %q is unavailable: %v", name, err)
		}
	}
}

func TestEmbeddedEnterpriseRuleEditorModuleIsAvailable(t *testing.T) {
	for _, name := range []string{"rule-editor.js", "workspace-summary.js", "telemetry.js", "auth-session.js", "account-menu.js", "console-navigation.js", "resource-mode.js"} {
		if _, err := Files.ReadFile(name); err != nil {
			t.Fatalf("embedded enterprise module %q is unavailable: %v", name, err)
		}
	}
}

func TestEmbeddedPagesUseSharedBlueBrandMark(t *testing.T) {
	mark, err := Files.ReadFile("reladock-mark.svg")
	if err != nil {
		t.Fatalf("embedded RelaDock brand mark is unavailable: %v", err)
	}
	for _, required := range [][]byte{[]byte("#2563EB"), []byte("#60A5FA"), []byte("#081A33")} {
		if !bytes.Contains(mark, required) {
			t.Fatalf("embedded brand mark is missing blue brand colour %q", required)
		}
	}
	if bytes.Contains(mark, []byte("#DAFA9C")) {
		t.Fatal("embedded brand mark still contains the former lime accent")
	}

	for _, name := range []string{"index.html", "admin.html"} {
		page, err := Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(page, []byte(`src="/reladock-mark.svg"`)) {
			t.Fatalf("%s does not render the shared RelaDock brand mark", name)
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
