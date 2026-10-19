// Package version exposes build version information. Version is set at link
// time by the build system via -ldflags "-X".
package version

// Name is the overall name of the application at the Git repository level.
const Name = "finite-resource-proportional-vertical-autoscaler"

// Description is an overall description of its function.
const Description = `Cluster-wide operator which scales pod resource requests in proportion to the finite resources of the cluster`

//nolint:gochecknoglobals // overridden at link time
var Version = "v0.0.0-dev"
