// Copyright (c) 2022 Multus Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a "Multi-plugin".The delegate concept referred from CNI project
// It reads other plugin netconf, and then invoke them, e.g.
// flannel or sriov plugin.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/containernetworking/cni/pkg/skel"
	cniversion "github.com/containernetworking/cni/pkg/version"

	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/multus"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/server/api"
)

func main() {
	// Init command line flags to clear vendored packages' one, especially in init()
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	// add version flag
	versionOpt := false
	flag.BoolVar(&versionOpt, "version", false, "Show application version")
	flag.BoolVar(&versionOpt, "v", false, "Show application version")

	flag.Parse()
	if versionOpt {
		fmt.Printf("multus-shim: %s\n", multus.PrintVersionString())
		return
	}

	skel.PluginMainFuncs(
		skel.CNIFuncs{
			Add: func(args *skel.CmdArgs) error {
				return api.CmdAdd(args)
			},
			Check: func(args *skel.CmdArgs) error {
				return api.CmdCheck(args)
			},
			Del: func(args *skel.CmdArgs) error {
				return api.CmdDel(args)
			},
			GC: func(args *skel.CmdArgs) error {
				return api.CmdGC(args)
			},
			Status: func(args *skel.CmdArgs) error {
				return api.CmdStatus(args)
			},
		},
		cniversion.All, "meta-plugin that delegates to other CNI plugins")
}
