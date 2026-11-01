// Package docs embeds the documentation the smates binary prints.
package docs

import _ "embed"

// Agents is docs/AGENTS.md, the guide for coding agents (`smates guide`).
//
//go:embed AGENTS.md
var Agents string
