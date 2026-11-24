// Command mono-jig is the personal track's harness: package jig's command
// line with jig.Personal (GitLab, GitHub, Claude and AWS; Claude Code;
// VS Code; microsandbox; Ubuntu). A work track is a command like this one,
// in its own repository, with its own Track (docs/library-split.md).
package main

import "mono-jig/jig"

func main() { jig.Main(jig.Personal()) }
