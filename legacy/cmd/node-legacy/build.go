package main

import "runtime/debug"

// forkVersion is the go-ethereum module this binary was built against, so a
// trace shows which discv5 a plain node ran.
func forkVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/ethereum/go-ethereum" {
			if d.Replace != nil {
				return d.Replace.Path + "@" + d.Replace.Version
			}
			return d.Version
		}
	}
	return ""
}
