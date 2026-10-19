// Package guide embeds the agent guide that the installed skill stub prints,
// so upgrading the binary updates what agents read.
package guide

import _ "embed"

//go:embed skill.md
var Skill string
