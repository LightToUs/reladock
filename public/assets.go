package assets

import "embed"

//go:embed index.html app.js app.css console-navigation.js workspace-summary.js telemetry.js node-monitoring.js rule-submission.js rule-editor.js account-tools.js business-tools.js operations-tools.js admin.html admin.js admin.css node-action-dialog.js node-bootstrap.js node-contact-status.js node-content-dialog.js node-editor.js node-withdrawal.js
var Files embed.FS
