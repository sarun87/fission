/*
Copyright 2016 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"log"
	"os/signal"
	"syscall"
	"os"
	"runtime/debug"

	"github.com/fission/fission/crd"
)

func dumpStackTrace() {
	debug.PrintStack()
}

func Start(port int) {
	// register signal handler for dumping stack trace.
	c := make(chan os.Signal, 1)
    	signal.Notify(c, syscall.SIGTERM)
    	go func() {
		<-c
		dumpStackTrace()
		os.Exit(1)
    	}()

	fc, _, apiExtClient, err := crd.MakeFissionClient()
	if err != nil {
		log.Fatalf("Failed to connect to K8s API: %v", err)
	}

	err = crd.EnsureFissionCRDs(apiExtClient)
	if err != nil {
		log.Fatalf("Failed to create fission CRDs: %v", err)
	}

	fc.WaitForCRDs()

	api, err := MakeAPI()
	if err != nil {
		log.Fatalf("Failed to start controller: %v", err)
	}
	api.Serve(port)
}
