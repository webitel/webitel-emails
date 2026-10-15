// Package model defines the service domain models.
package model

import "time"

// Service identity values used for registration and process metadata.
const (
	ServiceName      = "email-service"
	ServiceNamespace = "webitel"
)

// InstanceID identifies this process in Consul and as an Email Profile owner.
type InstanceID string

// Build metadata is overridden through linker flags for release binaries.
var (
	Version        = "0.0.0"
	Commit         = "hash"
	CommitDate     = time.Now().String()
	Branch         = "branch"
	BuildTimestamp = ""
)
