package deployment

import "embed"

//go:embed bootstrap-agent.sh
var files embed.FS
var BootstrapScript, _ = files.ReadFile("bootstrap-agent.sh")
