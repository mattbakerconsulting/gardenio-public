//go:build mage
// +build mage

package main

import (
	// mage:import
	build "github.com/grafana/grafana-plugin-sdk-go/build"
)

// Default configures the default Mage target: build the backend binary for all
// supported OS/arch into dist/ (the SDK's BuildAll). The chart only seeds the
// linux/amd64 binary (gpx_gardenio_assistant_app_linux_amd64); BuildAll keeps the
// local dev loop and any future arch identical to the upstream plugin scaffold.
var Default = build.BuildAll
