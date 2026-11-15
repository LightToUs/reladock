package assets

import "embed"

//go:embed index.html app.js app.css auth-session.js account-menu.js console-navigation.js workspace-summary.js telemetry.js node-monitoring.js rule-submission.js rule-editor.js account-tools.js business-tools.js operations-tools.js resource-mode.js plan-catalog.js admin.html admin.js admin.css lightsail-admin.js node-action-dialog.js node-bootstrap.js node-contact-status.js node-content-dialog.js node-editor.js node-visibility.js node-withdrawal.js source-flow-logging.js reladock-mark.svg
var Files embed.FS
