package assets

import "embed"

//go:embed index.html app.js app.css rule-submission.js account-tools.js business-tools.js operations-tools.js admin.html admin.js admin.css node-bootstrap.js
var Files embed.FS
