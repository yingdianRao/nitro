package main

import (
	"github.com/yingdianRao/nitro/linters/koanf"
	"github.com/yingdianRao/nitro/linters/pointercheck"
	"github.com/yingdianRao/nitro/linters/rightshift"
	"github.com/yingdianRao/nitro/linters/structinit"
	"golang.org/x/tools/go/analysis/multichecker"
)

func main() {
	multichecker.Main(
		koanf.Analyzer,
		pointercheck.Analyzer,
		rightshift.Analyzer,
		structinit.Analyzer,
	)
}
